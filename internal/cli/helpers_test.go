package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/store"
)

// update regenerates the golden files: `go test ./internal/cli -update`.
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// fixedNow is the instant every test runs at, so timestamps in golden output are stable.
var fixedNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// harness is one CLI invocation with everything injected: streams to assert on, XDG roots
// in a temp dir, and a clock that does not move.
type harness struct {
	t      *testing.T
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	paths  store.Paths
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	// Cleared so a developer's own shell cannot change what the tests assert. DOSSIER_TOKEN
	// in particular would silently override every stored profile.
	t.Setenv("DOSSIER_TOKEN", "")
	t.Setenv("DOSSIER_PROFILE", "")
	t.Setenv("NO_COLOR", "1")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	paths := store.PathsIn(t.TempDir())

	return &harness{
		t:      t,
		stdout: stdout,
		stderr: stderr,
		paths:  paths,
		app: &App{
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
			Paths:  paths,
			Now:    func() time.Time { return fixedNow },
		},
	}
}

// run executes the command tree with the given arguments and returns the exit code the
// process would have used. Driving the real root command rather than calling the RunE
// functions directly is the point: flag binding and argument validation are part of the
// contract being tested.
func (h *harness) run(args ...string) int {
	h.t.Helper()

	// Rebuilt per run so persistent flags do not leak between invocations.
	root := NewRoot(h.app)
	root.SetOut(h.stdout)
	root.SetErr(h.stderr)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())
	if err == nil {
		return 0
	}

	var cliErr *Error
	if !errors.As(err, &cliErr) {
		cliErr = Classify(err)
	}
	// Mirrors what main does, so the tests assert on the bytes a user would see.
	h.stderr.WriteString(cliErr.Message)
	return cliErr.Code
}

func (h *harness) stdinString(value string) {
	h.app.Stdin = strings.NewReader(value)
}

// seedProfile writes a credentials file the way login would.
func (h *harness) seedProfile(name string, profile store.Profile) {
	h.t.Helper()

	creds, err := store.LoadCredentials(h.paths)
	if err != nil {
		h.t.Fatalf("LoadCredentials: %v", err)
	}
	creds.Put(name, profile)
	if err := creds.Save(h.paths); err != nil {
		h.t.Fatalf("Save: %v", err)
	}
	// The App caches these per run, and a test that seeds after a previous run would
	// otherwise see the stale copy.
	h.app.creds = nil
	h.app.config = nil
}

func (h *harness) seedDefaultProfile(name string) {
	h.t.Helper()

	config, err := store.LoadConfig(h.paths)
	if err != nil {
		h.t.Fatalf("LoadConfig: %v", err)
	}
	config.DefaultProfile = name
	if err := config.Save(h.paths); err != nil {
		h.t.Fatalf("Save: %v", err)
	}
	h.app.config = nil
}

// reset clears the buffers and the per-run caches between invocations in one test.
func (h *harness) reset() {
	h.stdout.Reset()
	h.stderr.Reset()
	h.app.creds = nil
	h.app.config = nil
	h.app.ProfileFlag = ""
	h.app.HostFlag = ""
	h.app.JSON = false
	h.app.NoColor = false
	h.app.NoWait = false
	h.app.Quiet = false
	h.app.Timeout = 0
}

// fakeAPI is an httptest server that fails the test if anything outside /api/v1 is
// requested — the §9.1 fake that "500s anything else, and asserts it was never called".
func fakeAPI(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, handler := range routes {
		mux.HandleFunc(pattern, handler)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1") {
			t.Errorf("the CLI requested %q, which is outside /api/v1", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// jsonResponse writes a status and body.
// readAll drains a request body, failing the test rather than returning an error — a
// handler that cannot read what it was sent has nothing useful left to assert.
func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	return body
}

// hijackAndClose takes the connection and drops it without a reply.
//
// This is the only way to produce the failure the mint ledger exists for: the request was
// received in full, and the client will never learn what became of it. A 500 is a
// different thing — the server answered.
func hijackAndClose(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func jsonResponse(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// errorEnvelope writes an API error envelope.
//
// Marshalled rather than concatenated, which it used to be. The API's real hints contain
// double quotes — `Check the "Retry-After" header (seconds) and back off.` is one — and
// pasting one in produced invalid JSON that the client could not decode, so the envelope
// arrived with an empty code and mapped to exit 1. The test that hit this looked like a
// broken exit-code mapping and was a broken fixture.
func errorEnvelope(status int, code, message, hint string) http.HandlerFunc {
	body, err := json.Marshal(map[string]any{
		"error": map[string]string{"code": code, "message": message, "hint": hint},
	})
	if err != nil {
		panic(err)
	}
	return jsonResponse(status, string(body))
}

// schemaFixture is a discovery document shaped like the real one.
//
// It is no longer taken on trust: contract_test.go asserts every closed vocabulary here
// against a live server, so a fake that drifts from the API fails CI rather than quietly
// certifying the wrong thing. The endpoint and error-code lists are deliberate subsets —
// enough rows to render — and the contract suite checks those as subsets. Everything
// else, including the expiry prose, is asserted verbatim.
const schemaFixture = `{
  "endpoints": [
    {"method":"GET","path":"/api/v1/schema","scope":null},
    {"method":"GET","path":"/api/v1/fields","scope":"fields:read"},
    {"method":"POST","path":"/api/v1/shares","scope":"shares:mint"}
  ],
  "scopes": ["fields:read","fields:write","documents:write","shares:mint","shares:read"],
  "field_categories": ["identity","travel","tax","health","contact"],
  "field_statuses": ["empty","active","pending","withdrawn"],
  "share_states": ["live","expiring","expired","revoked"],
  "error_codes": [
    {"code":"unauthenticated","status":401,"message":"Missing or invalid API token.","hint":"Send \"Authorization: Bearer dsk_<token>\" with a token that is not expired or revoked."},
    {"code":"insufficient_scope","status":403,"message":"This token does not have the scope this endpoint requires.","hint":"Mint a new token with the required scope from the Settings page — scopes are fixed at mint."},
    {"code":"expiry_required","status":422,"message":"Expiry is required. Pass \"expires_at\" explicitly.","hint":"Pass an ISO 8601 timestamp, or an explicit null for the labelled no-expiry exception."},
    {"code":"rate_limited","status":429,"message":"Too many requests.","hint":"Check the \"Retry-After\" header (seconds) and back off."}
  ],
  "limits": {
    "pagination": {"fields": {"default_limit": 50, "max_limit": 200}},
    "uploads": {"max_file_bytes": 26214400}
  },
  "expiry": {
    "required": true,
    "presets": ["PT24H","P7D","P30D","P90D"],
    "note": "Presets are ISO 8601 durations, not timestamps. Compute expires_at yourself: now + preset (e.g. now + \"PT24H\" = 24 hours from now), and send that timestamp.",
    "no_expiry": {"value": null, "label": "No expiry", "note": "This is the exception: the link works until you revoke it by hand."}
  }
}`

// assertGolden compares output against testdata/golden/<name>, rewriting it under
// -update.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v\n\nRun `go test ./internal/cli -update` to create it. Got:\n%s", path, err, got)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}
