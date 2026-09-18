package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// dossierFixture is the body of a successful open, shaped like the live one.
//
// It carries a redacted row and a document deliberately: the withheld bar and the id
// column are the two parts of this rendering that exist for reasons beyond display.
const dossierFixture = `{
  "dossier": {
    "title": "Lease application",
    "state": "live",
    "expires_at": "2026-09-22T03:00:00Z",
    "holder": {"name": "Marisol Vega Ortiz", "nationality": "MEX"},
    "released_fields": [
      {"label": "Legal name (as printed)", "value": "VEGA ORTIZ, MARISOL", "country": null},
      {"label": "Date of birth", "value": "1990-04-17", "country": null},
      {"label": "U.S. Passport", "value": "X12345678", "country": "USA"}
    ],
    "redacted_fields": [
      {"label": "Social Security Number", "mask_length": 11}
    ],
    "documents": [
      {"id": 43, "filename": "passport-scan.pdf", "content_type": "application/pdf",
       "path": "/api/v1/dossiers/k7m2p9qrx/documents/43"}
    ]
  }
}`

const viewPath = "/api/v1/dossiers/K7M2P9QRX/view"
const documentPath = "/api/v1/dossiers/K7M2P9QRX/documents/43"

// openHarness wires a CLI against a fake API, counting every request that reaches it.
func openHarness(t *testing.T, routes map[string]http.HandlerFunc) (*harness, *httptest.Server, *atomic.Int32) {
	t.Helper()

	var requests atomic.Int32
	counted := map[string]http.HandlerFunc{}
	for pattern, handler := range routes {
		counted[pattern] = func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			handler(w, r)
		}
	}

	server := fakeAPI(t, counted)
	h := newHarness(t)
	return h, server, &requests
}

// --- §8.2, the one-request rule -------------------------------------------------------

// TestOpenSendsExactlyOneView is the rule the whole recipient side is built around.
func TestOpenSendsExactlyOneView(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})

	h.stdinString("539993\n")
	if code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL); code != exitcode.OK {
		t.Fatalf("exit = %d, stderr = %s", code, h.stderr)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("sent %d requests; §8.2 allows exactly one", got)
	}
}

// TestOpenWithDocumentSendsNoView is the sharper half of the same rule.
//
// The id can only have come from an earlier open, so a `view` here to re-learn it would
// itself be an open — and on a burn-after-read dossier, the open. The document endpoint
// is addressed directly.
func TestOpenWithDocumentSendsNoView(t *testing.T) {
	var views atomic.Int32
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: func(w http.ResponseWriter, r *http.Request) {
			views.Add(1)
			jsonResponse(http.StatusOK, dossierFixture)(w, r)
		},
		documentPath: jsonResponse(http.StatusOK, `{"url":"https://example.invalid/x","expires_in":300}`),
	})

	h.stdinString("539993\n")
	h.run("open", "K7M2P9QRX", "--document", "43", "--url-only", "--pin-stdin", "--host", server.URL)

	if got := views.Load(); got != 0 {
		t.Errorf("--document sent %d views; it must send none at all", got)
	}
}

// TestOpenDoesNotRetryARateLimitedView. A 429 is the one failure this client retries
// everywhere else, and `view` is where it must not: the CLI cannot tell a rate limit that
// rejected the request from one that arrived late, and a burn dossier only opens once.
func TestOpenDoesNotRetryARateLimitedView(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		viewPath: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			errorEnvelope(http.StatusTooManyRequests, "rate_limited",
				"Too many requests.", `Check the "Retry-After" header (seconds) and back off.`)(w, r)
		},
	})

	h.stdinString("539993\n")
	code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if got := requests.Load(); got != 1 {
		t.Errorf("sent %d requests after a 429; the view must be sent once and only once", got)
	}
	if code != exitcode.TryLater {
		t.Errorf("exit = %d, want %d", code, exitcode.TryLater)
	}
}

// TestOpenSaysTheOutcomeIsUnknownAfterATransportFailure. The sentence that makes the
// one-request rule usable rather than merely safe.
func TestOpenSaysTheOutcomeIsUnknownAfterATransportFailure(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		viewPath: func(w http.ResponseWriter, r *http.Request) {
			_ = readAll(t, r)
			hijackAndClose(t, w)
		},
	})

	h.stdinString("539993\n")
	code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if code != exitcode.Unexpected {
		t.Errorf("exit = %d, want %d", code, exitcode.Unexpected)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("sent %d requests; a dropped connection must not be retried", got)
	}
	if !strings.Contains(h.stderr.String(), "it may now be consumed") {
		t.Errorf("the ambiguous-outcome warning is missing:\n%s", h.stderr)
	}
}

// TestViewCarriesNothingThatWouldMakeItReplayable guards the half of the one-request rule
// this package cannot enforce.
//
// api.Request.Once stops *our* retries. It cannot stop net/http's, which replays a
// request on a reused idle connection when the connection dies before any reply — and
// TestNetHTTPReplaysOnlyTheKeyedPost shows the trigger is an idempotency header on a POST.
// `view` must therefore never grow one.
//
// Two things make this safe rather than merely likely, and both are worth stating because
// a future change could quietly remove either. The header is absent, and `open` issues
// exactly one request per process — so its connection is always fresh, and Go does not
// replay on a fresh connection at all. The header is the property under test here; the
// single request is the property every other test in this file asserts.
func TestViewCarriesNothingThatWouldMakeItReplayable(t *testing.T) {
	var headers http.Header
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: func(w http.ResponseWriter, r *http.Request) {
			headers = r.Header.Clone()
			jsonResponse(http.StatusOK, dossierFixture)(w, r)
		},
	})

	h.stdinString("539993\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	for _, name := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		if got := headers.Get(name); got != "" {
			t.Errorf("view carries %s: %q — net/http would then replay it on a dropped "+
				"connection, and for a burn dossier the replay is a second open", name, got)
		}
	}
}

// --- refusals -------------------------------------------------------------------------

func TestOpenWrongPinAddsTheLockWarning(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: errorEnvelope(http.StatusUnauthorized, "pin_incorrect",
			"That PIN doesn’t match. The attempt has been logged.",
			"Ask the holder to reissue the PIN if you are not sure of it."),
	})

	h.stdinString("000000\n")
	code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if code != exitcode.PinRefused {
		t.Errorf("exit = %d, want %d", code, exitcode.PinRefused)
	}
	stderr := h.stderr.String()
	// The API's copy, verbatim, and then the one line it does not say.
	if !strings.Contains(stderr, "That PIN doesn’t match.") {
		t.Errorf("the API's own message is missing:\n%s", stderr)
	}
	if !strings.Contains(stderr, lockWarning) {
		t.Errorf("the lock warning is missing:\n%s", stderr)
	}
}

// A second guess is a second invocation, deliberately: re-prompting would let one command
// walk a recipient into the five-attempt lock.
func TestOpenDoesNotRepromptAfterAWrongPin(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		viewPath: errorEnvelope(http.StatusUnauthorized, "pin_incorrect",
			"That PIN doesn’t match. The attempt has been logged.", "Ask the holder."),
	})

	h.stdinString("000000\n111111\n222222\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if got := requests.Load(); got != 1 {
		t.Errorf("made %d attempts; a wrong PIN must cost exactly one", got)
	}
}

func TestOpenRefusalsMapToTheirExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		message string
		want    int
	}{
		{"locked", http.StatusLocked, "pin_locked",
			"Too many incorrect attempts. This dossier is locked for about 15 more minutes.", exitcode.PinRefused},
		{"no pin", http.StatusUnauthorized, "pin_required",
			"A PIN is required to view this dossier.", exitcode.PinRefused},
		{"expired", http.StatusGone, "share_expired",
			"This dossier has expired.", exitcode.ShareClosed},
		{"revoked", http.StatusGone, "share_revoked",
			"This dossier has been revoked.", exitcode.ShareClosed},
		{"unknown token", http.StatusNotFound, "not_found",
			"Not found.", exitcode.NotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, server, _ := openHarness(t, map[string]http.HandlerFunc{
				viewPath: errorEnvelope(tc.status, tc.code, tc.message, "hint"),
			})

			h.stdinString("539993\n")
			code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			// Verbatim, always: the lock's live countdown is the server's to state.
			if !strings.Contains(h.stderr.String(), tc.message) {
				t.Errorf("the API's message is missing:\n%s", h.stderr)
			}
		})
	}
}

// A 410 carries no dossier, and the CLI must not invent one.
func TestOpenPrintsNoDossierDataWhenClosed(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: errorEnvelope(http.StatusGone, "share_revoked",
			"This dossier has been revoked.", "Revocation cannot be undone over this API."),
	})

	h.stdinString("539993\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if h.stdout.Len() != 0 {
		t.Errorf("a closed dossier wrote to stdout:\n%s", h.stdout)
	}
}

// --- §8.3, documents --------------------------------------------------------------------

// TestOpenDocumentNotFoundAddsTheBurnLine. The API answers an unknown id and a burn
// dossier's document with one identical 404, so the line is added to both — phrased as a
// general fact rather than a claim about this request.
func TestOpenDocumentNotFoundAddsTheBurnLine(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		documentPath: errorEnvelope(http.StatusNotFound, "not_found", "Not found.", "Check the id."),
	})

	h.stdinString("245423\n")
	code := h.run("open", "K7M2P9QRX", "--document", "43", "--out", filepath.Join(t.TempDir(), "x.pdf"),
		"--pin-stdin", "--host", server.URL)

	if code != exitcode.NotFound {
		t.Errorf("exit = %d, want %d", code, exitcode.NotFound)
	}
	if !strings.Contains(h.stderr.String(), burnDocumentNote) {
		t.Errorf("§8.3's line is missing:\n%s", h.stderr)
	}
}

// TestOpenDownloadsThroughASeparateTransport is the test that documents the one
// deliberate exception to "every request is under /api/v1".
//
// fakeAPI fails the test if the CLI touches any other path on the API host. The signed
// URL is served by a *different* server here, which is what production looks like too —
// the bucket is not the API. So the invariant holds as stated: every request the CLI
// composes is under /api/v1, and the only other request it makes is one it was handed.
func TestOpenDownloadsThroughASeparateTransport(t *testing.T) {
	const payload = "%PDF-1.4\nnot really a passport\n"

	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			// A credential sent to object storage is a credential given to whoever the
			// URL points at. The capability is the URL.
			t.Errorf("the download carried an Authorization header: %q", got)
		}
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(bucket.Close)

	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		documentPath: func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Dossier-Pin"); got != "245423" {
				t.Errorf("X-Dossier-Pin = %q; the PIN travels in the header here", got)
			}
			jsonResponse(http.StatusOK, `{"url":"`+bucket.URL+`/blob","expires_in":300}`)(w, r)
		},
	})

	out := filepath.Join(t.TempDir(), "passport.pdf")
	h.stdinString("245423\n")
	if code := h.run("open", "K7M2P9QRX", "--document", "43", "--out", out,
		"--pin-stdin", "--host", server.URL); code != exitcode.OK {
		t.Fatalf("exit = %d, stderr = %s", code, h.stderr)
	}

	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the download: %v", err)
	}
	if string(saved) != payload {
		t.Errorf("saved %q, want %q", saved, payload)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// An identity document should not land world-readable, even briefly.
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %04o, want 0600", mode)
	}
}

func TestOpenUrlOnlyPrintsTheLinkAndFetchesNothing(t *testing.T) {
	var fetched atomic.Bool
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Store(true)
	}))
	t.Cleanup(bucket.Close)

	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		documentPath: jsonResponse(http.StatusOK, `{"url":"`+bucket.URL+`/blob","expires_in":300}`),
	})

	h.stdinString("245423\n")
	if code := h.run("open", "K7M2P9QRX", "--document", "43", "--url-only",
		"--pin-stdin", "--host", server.URL); code != exitcode.OK {
		t.Fatalf("exit = %d, stderr = %s", code, h.stderr)
	}

	if fetched.Load() {
		t.Error("--url-only followed the link; it must only print it")
	}
	if got := strings.TrimSpace(h.stdout.String()); got != bucket.URL+"/blob" {
		t.Errorf("stdout = %q, want the URL alone", got)
	}
}

// Writing bytes of a scanned passport into a terminal by default is one keystroke from a
// corrupted session and an identity document in the scrollback.
func TestOpenDocumentRefusesWithoutSomewhereToPutIt(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		documentPath: jsonResponse(http.StatusOK, `{"url":"https://example.invalid/x","expires_in":300}`),
	})

	h.stdinString("245423\n")
	code := h.run("open", "K7M2P9QRX", "--document", "43", "--pin-stdin", "--host", server.URL)

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests before refusing; a usage error must cost nothing", got)
	}
}

func TestOpenRefusesDocumentFlagsWithoutADocument(t *testing.T) {
	for _, flag := range []string{"--url-only", "--out"} {
		t.Run(flag, func(t *testing.T) {
			h, server, requests := openHarness(t, map[string]http.HandlerFunc{
				viewPath: jsonResponse(http.StatusOK, dossierFixture),
			})

			args := []string{"open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL, flag}
			if flag == "--out" {
				args = append(args, "x.pdf")
			}
			h.stdinString("539993\n")

			if code := h.run(args...); code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("sent %d requests before refusing", got)
			}
		})
	}
}

// --- the PIN --------------------------------------------------------------------------

// TestOpenTakesNoPinArgument. The flag set is the enforcement: there is no --pin and no
// second positional, so there is no spelling of this command that puts a PIN in argv,
// where ps and the shell history would both find it.
func TestOpenTakesNoPinArgument(t *testing.T) {
	cmd := newOpenCmd(newHarness(t).app)

	if flag := cmd.Flags().Lookup("pin"); flag != nil {
		t.Error("open has a --pin flag; the PIN must never be an argument")
	}
	if err := cmd.Args(cmd, []string{"K7M2P9QRX", "539993"}); err == nil {
		t.Error("open accepted a second positional argument, which could be a PIN")
	}
}

func TestOpenWithNoTerminalAndNoPinStdinSaysHowToPipeIt(t *testing.T) {
	h, server, requests := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})
	h.app.IsInteractive = func() bool { return false }

	code := h.run("open", "K7M2P9QRX", "--host", server.URL)

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests without a PIN", got)
	}
	if !strings.Contains(h.stderr.String(), "--pin-stdin") {
		t.Errorf("the message does not say how to supply the PIN:\n%s", h.stderr)
	}
}

func TestOpenSendsThePinInTheBodyNotTheUrl(t *testing.T) {
	var seenURL, seenBody string
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: func(w http.ResponseWriter, r *http.Request) {
			seenURL = r.URL.String()
			seenBody = string(readAll(t, r))
			jsonResponse(http.StatusOK, dossierFixture)(w, r)
		},
	})

	h.stdinString("539 993\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if strings.Contains(seenURL, "539") {
		t.Errorf("the PIN reached the URL: %q", seenURL)
	}
	var sent struct {
		Pin string `json:"pin"`
	}
	if err := json.Unmarshal([]byte(seenBody), &sent); err != nil {
		t.Fatalf("the body was not JSON: %v", err)
	}
	// Whitespace trimmed at the edges only. The API's own comparison is
	// whitespace-insensitive, and a PIN is displayed to the recipient as "539 993".
	if sent.Pin != "539 993" {
		t.Errorf("pin = %q, want the PIN as typed", sent.Pin)
	}
}

// --- rendering --------------------------------------------------------------------------

func TestOpenRendersTheDossier(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})

	h.stdinString("539993\n")
	if code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL); code != exitcode.OK {
		t.Fatalf("exit = %d, stderr = %s", code, h.stderr)
	}

	assertGolden(t, "open-dossier.txt", h.stdout.String())
}

// Rule 9 is honesty to both sides. The holder's audit log records this open; the
// recipient is told so rather than left to assume otherwise.
func TestOpenTellsTheRecipientItWasRecorded(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})

	h.stdinString("539993\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	if !strings.Contains(h.stderr.String(), "This open has been recorded for the holder.") {
		t.Errorf("the recipient was not told the open is recorded:\n%s", h.stderr)
	}
}

// Rule 5, from the client's side. A withheld field has no value here to reveal — the
// server sends a length and nothing else — and the bar must not be reconstructible into
// one. This asserts the rendering carries the label and a bar, and no third thing.
func TestOpenWithheldRowsCarryOnlyALengthAndABar(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})

	h.stdinString("539993\n")
	h.run("open", "K7M2P9QRX", "--pin-stdin", "--host", server.URL)

	var withheld string
	for _, line := range strings.Split(h.stdout.String(), "\n") {
		if strings.Contains(line, "Social Security Number") {
			withheld = line
		}
	}
	if withheld == "" {
		t.Fatalf("the withheld row is missing:\n%s", h.stdout)
	}
	if strings.Count(withheld, "█") != 11 {
		t.Errorf("the bar is not the server's mask_length: %q", withheld)
	}
	// The fixture the API would have redacted. If this ever appears, the leak is upstream
	// and this test is the alarm.
	if strings.Contains(h.stdout.String(), "123-45-6789") {
		t.Errorf("a withheld value reached the output:\n%s", h.stdout)
	}
}

func TestOpenJSONIsTheServersBytes(t *testing.T) {
	h, server, _ := openHarness(t, map[string]http.HandlerFunc{
		viewPath: jsonResponse(http.StatusOK, dossierFixture),
	})

	h.stdinString("539993\n")
	if code := h.run("open", "K7M2P9QRX", "--pin-stdin", "--json", "--host", server.URL); code != exitcode.OK {
		t.Fatalf("exit = %d, stderr = %s", code, h.stderr)
	}

	var got, want any
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout was not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(dossierFixture), &want); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !jsonEqual(got, want) {
		t.Errorf("--json did not emit the server's document verbatim:\n%s", h.stdout)
	}
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
