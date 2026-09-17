package cli

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// theSecret is the value every test in this file sends. Nothing may print it.
const theSecret = "ROGV880801MDFSZL09"

const createdFieldJSON = `{"id":118,"label":"CURP","category":"identity","country":"MEX","sensitivity":2,"status":"active","has_document":false,"documents":[]}`

const attachedDocumentJSON = `{"document":{"id":43,"field_id":118,"byte_size":481203,"content_type":"application/pdf"}}`

// writeHarness is a harness with a profile and a schema, for the write commands.
func newWriteHarness(t *testing.T, routes map[string]http.HandlerFunc) *harness {
	t.Helper()

	h := newHarness(t)
	if _, taken := routes["/api/v1/schema"]; !taken {
		routes["/api/v1/schema"] = jsonResponse(http.StatusOK, schemaFixture)
	}
	server := fakeAPI(t, routes)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: "dsk_write"})
	h.seedDefaultProfile("default")
	return h
}

// The M2 done-when: a value passed as an argument is refused with exit 2, and nothing is
// sent. Both spellings a holder would reach for are covered — the positional and the
// obvious --value — because refusing one and accepting the other would be worse than
// refusing neither.
func TestFieldsCreateRefusesTheValueOnTheCommandLine(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"positional", []string{"fields", "create", "--label", "CURP", theSecret}},
		{"--value flag", []string{"fields", "create", "--label", "CURP", "--value", theSecret}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// No routes at all: fakeAPI fails the test if anything is requested, which is
			// how "nothing was sent" is asserted rather than assumed.
			h := newWriteHarness(t, map[string]http.HandlerFunc{})
			h.app.Stdin = strings.NewReader("")

			if code := h.run(testCase.args...); code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}

			out := h.stdout.String() + h.stderr.String()
			if strings.Contains(out, theSecret) {
				t.Errorf("the refusal echoed the secret back:\n%s", out)
			}
			for _, want := range []string{"shell history", "--value-stdin", "--value-file"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestFieldsCreateReadsTheValueFromEachPermittedSource(t *testing.T) {
	valueFile := filepath.Join(t.TempDir(), "curp.txt")
	// With a trailing newline, the shape `printf '%s\n' ... > file` leaves. It must not
	// reach the server as part of the value.
	if err := os.WriteFile(valueFile, []byte(theSecret+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name        string
		args        []string
		stdin       string
		interactive bool
	}{
		{name: "--value-stdin", args: []string{"--value-stdin"}, stdin: theSecret + "\n"},
		{name: "--value-file", args: []string{"--value-file", valueFile}},
		{name: "hidden prompt", stdin: theSecret + "\n", interactive: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var sent map[string]any
			h := newWriteHarness(t, map[string]http.HandlerFunc{
				"POST /api/v1/fields": func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					sent = decodeJSON(t, body)
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, createdFieldJSON)
				},
			})
			h.app.Stdin = strings.NewReader(testCase.stdin)
			if testCase.interactive {
				h.app.IsInteractive = func() bool { return true }
			}

			args := append([]string{"fields", "create", "--label", "CURP"}, testCase.args...)
			if code := h.run(args...); code != 0 {
				t.Fatalf("exit = %d\n%s", code, h.stderr.String())
			}

			if sent["value"] != theSecret {
				t.Errorf("value sent = %q, want %q", sent["value"], theSecret)
			}
		})
	}
}

// Rule 5's discipline applied to the client: the value is in this process, and the one
// place it is allowed to go is the request body.
func TestFieldsCreateNeverEchoesTheValue(t *testing.T) {
	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields": jsonResponse(http.StatusCreated, createdFieldJSON),
	})
	h.app.Stdin = strings.NewReader(theSecret + "\n")

	if code := h.run("fields", "create", "--label", "CURP", "--value-stdin"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}

	if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, theSecret) {
		t.Errorf("the value reached the terminal:\n%s", out)
	}
}

// Under --json the body is the server's own bytes, and the server does not return the
// value — so this asserts the passthrough is a passthrough, not a re-marshalling that
// could pick up the request's fields on the way.
func TestFieldsCreateJSONIsTheServersBytesAndCarriesNoValue(t *testing.T) {
	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields": jsonResponse(http.StatusCreated, createdFieldJSON),
	})
	h.app.Stdin = strings.NewReader(theSecret + "\n")

	if code := h.run("--json", "fields", "create", "--label", "CURP", "--value-stdin"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}

	if got := strings.TrimSpace(h.stdout.String()); got != createdFieldJSON {
		t.Errorf("stdout is not the server's body verbatim:\n got: %s\nwant: %s", got, createdFieldJSON)
	}
	if strings.Contains(h.stdout.String(), theSecret) {
		t.Error("the value appeared in the --json output")
	}
}

func TestFieldsCreateValidatesTheCategoryAgainstTheSchema(t *testing.T) {
	// "documents" is the interesting one: it is a real member of the model's category
	// list and a virtual vault filter, but the API refuses it as a create input. The
	// schema is what knows that, and the CLI refuses before sending because of it.
	h := newWriteHarness(t, map[string]http.HandlerFunc{})
	h.app.Stdin = strings.NewReader(theSecret + "\n")

	if code := h.run("fields", "create", "--label", "CURP", "--category", "documents", "--value-stdin"); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if out := h.stderr.String(); !strings.Contains(out, "identity, travel, tax, health, contact") {
		t.Errorf("the refusal did not name the vocabulary:\n%s", out)
	}
}

// A script with no value source must be refused, not left blocking on a stdin nobody is
// going to type into — which is how a CI job hangs until its own timeout.
func TestFieldsCreateRefusesWhenThereIsNoValueAndNoTerminal(t *testing.T) {
	h := newWriteHarness(t, map[string]http.HandlerFunc{})
	h.app.IsInteractive = func() bool { return false }

	if code := h.run("fields", "create", "--label", "CURP"); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if out := h.stderr.String(); !strings.Contains(out, "--value-stdin") {
		t.Errorf("the refusal does not say what to do:\n%s", out)
	}
}

func TestFieldsCreateRefusesTwoValueSources(t *testing.T) {
	h := newWriteHarness(t, map[string]http.HandlerFunc{})
	if code := h.run("fields", "create", "--label", "C", "--value-stdin", "--value-file", "/dev/null"); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
}

func TestFieldsCreateRendersTheCreatedRow(t *testing.T) {
	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields": jsonResponse(http.StatusCreated, createdFieldJSON),
	})
	h.app.Stdin = strings.NewReader(theSecret + "\n")

	if code := h.run("fields", "create", "--label", "CURP", "--value-stdin"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}
	assertGolden(t, "fields_create.txt", h.stdout.String())
}

// The duplicate-label 422 is the failure a retrying agent is most likely to meet, and the
// API's message is the whole explanation — so it is rendered verbatim, and the exit code
// is the one a script branches on.
func TestFieldsCreateRendersTheAPIsRefusalVerbatim(t *testing.T) {
	const message = "You already have a field named “CURP”."
	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields": errorEnvelope(http.StatusUnprocessableEntity,
			"validation_failed", message, "See \\\"message\\\" for the specific validation that failed."),
	})
	h.app.Stdin = strings.NewReader(theSecret + "\n")

	want, _ := exitcode.FromErrorCode("validation_failed")
	if code := h.run("fields", "create", "--label", "CURP", "--value-stdin"); code != want {
		t.Errorf("exit = %d, want %d", code, want)
	}
	if out := h.stderr.String(); !strings.Contains(out, message) {
		t.Errorf("the API's own message was not rendered:\n%s", out)
	}
}

func TestDocumentsAttachSendsTheFileAndRendersTheDocument(t *testing.T) {
	path := writeFile(t, "passport.pdf", "%PDF-1.4 body")

	var (
		filename    string
		contentType string
	)
	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields/118/documents": func(w http.ResponseWriter, r *http.Request) {
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
			if err != nil {
				t.Errorf("reading the part: %v", err)
				return
			}
			filename = part.FileName()
			contentType = part.Header.Get("Content-Type")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, attachedDocumentJSON)
		},
	})

	if code := h.run("documents", "attach", "118", path); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}

	if filename != "passport.pdf" {
		t.Errorf("filename = %q — the base name is sent, never the path", filename)
	}
	if contentType != "application/pdf" {
		t.Errorf("declared type = %q, want application/pdf from the extension", contentType)
	}
	assertGolden(t, "documents_attach.txt", h.stdout.String())
}

// The M2 done-when: a 26 MB file is refused locally, quoting the schema's cap, and
// nothing is uploaded.
func TestDocumentsAttachRefusesAnOversizedFileBeforeUploading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.pdf")
	// 26 MiB against the schema fixture's 25 MiB cap.
	if err := os.WriteFile(path, make([]byte, 26*1024*1024), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// No upload route: the fake fails the test if the CLI sends the body anyway.
	h := newWriteHarness(t, map[string]http.HandlerFunc{})

	if code := h.run("documents", "attach", "118", path); code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}

	out := h.stderr.String()
	// Both numbers, so the refusal is a comparison the reader can make rather than an
	// assertion they have to take on faith.
	for _, want := range []string{"26.0 MiB", "25.0 MiB", "Nothing was uploaded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
}

// The cap is the server's. When the schema does not publish one, the upload goes ahead
// rather than being refused against a number this binary invented.
func TestDocumentsAttachUploadsWhenTheSchemaPublishesNoCap(t *testing.T) {
	path := writeFile(t, "scan.pdf", "body")
	uploaded := false

	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, `{"scopes":[],"limits":{}}`),
		"POST /api/v1/fields/118/documents": func(w http.ResponseWriter, r *http.Request) {
			uploaded = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, attachedDocumentJSON)
		},
	})

	if code := h.run("documents", "attach", "118", path); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}
	if !uploaded {
		t.Error("the upload was refused although the schema published no cap")
	}
}

// The M2 done-when: a 415 renders the API's hint. The allow-list is not in the schema
// (the plan's gap 5), so the server's refusal is the only place that knowledge exists and
// dropping its hint would leave the holder with nothing to act on.
func TestDocumentsAttachRendersThe415Hint(t *testing.T) {
	path := writeFile(t, "notes.pdf", "not really a pdf")
	const hint = "Send multipart/form-data with an allow-listed file content type."

	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields/118/documents": errorEnvelope(http.StatusUnsupportedMediaType,
			"unsupported_media_type", "Unsupported content type.", hint),
	})

	want, _ := exitcode.FromErrorCode("unsupported_media_type")
	if code := h.run("documents", "attach", "118", path); code != want {
		t.Errorf("exit = %d, want %d", code, want)
	}
	if out := h.stderr.String(); !strings.Contains(out, hint) {
		t.Errorf("the API's hint was dropped:\n%s", out)
	}
}

func TestDocumentsAttachDerivesTheTypeFromTheExtension(t *testing.T) {
	cases := map[string]string{
		"scan.pdf":  "application/pdf",
		"scan.JPG":  "image/jpeg",
		"scan.jpeg": "image/jpeg",
		"scan.png":  "image/png",
		"scan.webp": "image/webp",
		"scan.heic": "image/heic",
		"scan.heif": "image/heif",
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := contentTypeFor(filepath.Join("/tmp", name))
			if err != nil {
				t.Fatalf("contentTypeFor(%q): %v", name, err)
			}
			if got != want {
				t.Errorf("contentTypeFor(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

func TestDocumentsAttachRefusesAnUndeclarableFile(t *testing.T) {
	cases := []struct {
		name string
		file string
	}{
		{"unknown extension", "notes.txt"},
		{"no extension", "passport"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeFile(t, testCase.file, "x")
			h := newWriteHarness(t, map[string]http.HandlerFunc{})

			if code := h.run("documents", "attach", "118", path); code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}
			if out := h.stderr.String(); !strings.Contains(out, "--content-type") {
				t.Errorf("the refusal does not offer the way through:\n%s", out)
			}
		})
	}
}

// --content-type exists so a file whose name says nothing can still be sent. The server
// allow-lists on the declaration, so this is the only route for such a file.
func TestDocumentsAttachHonoursAnExplicitContentType(t *testing.T) {
	path := writeFile(t, "scan", "body")
	var declared string

	h := newWriteHarness(t, map[string]http.HandlerFunc{
		"POST /api/v1/fields/118/documents": func(w http.ResponseWriter, r *http.Request) {
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			part, _ := multipart.NewReader(r.Body, params["boundary"]).NextPart()
			declared = part.Header.Get("Content-Type")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, attachedDocumentJSON)
		},
	})

	if code := h.run("documents", "attach", "118", path, "--content-type", "application/pdf"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, h.stderr.String())
	}
	if declared != "application/pdf" {
		t.Errorf("declared type = %q", declared)
	}
}

func TestDocumentsAttachRefusesABadTarget(t *testing.T) {
	path := writeFile(t, "scan.pdf", "x")
	directory := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		{"a non-numeric id", []string{"documents", "attach", "abc", path}},
		{"a missing file", []string{"documents", "attach", "118", filepath.Join(directory, "nope.pdf")}},
		{"a directory", []string{"documents", "attach", "118", directory}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newWriteHarness(t, map[string]http.HandlerFunc{})
			if code := h.run(testCase.args...); code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}
		})
	}
}

// Both writes refuse before sending when there is no credential, rather than spending a
// unit of the per-IP failed-auth budget to learn what the CLI already knew.
func TestWritesRefuseWithoutAToken(t *testing.T) {
	path := writeFile(t, "scan.pdf", "x")

	cases := [][]string{
		{"fields", "create", "--label", "CURP", "--value-stdin"},
		{"documents", "attach", "118", path},
	}

	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			h := newHarness(t)
			h.app.Stdin = strings.NewReader(theSecret + "\n")

			if code := h.run(args...); code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}
			if out := h.stderr.String(); !strings.Contains(out, "No API token") {
				t.Errorf("stderr = %q", out)
			}
		})
	}
}

// A transport failure is the ambiguous one: the server may have acted and the reply been
// lost. Neither command retries it, and both say so — an agent that reads "failed" and
// resends would duplicate a document silently.
func TestWritesExplainThemselvesAfterAnAmbiguousFailure(t *testing.T) {
	path := writeFile(t, "scan.pdf", "x")

	cases := []struct {
		name  string
		route string
		args  []string
	}{
		{"fields create", "POST /api/v1/fields", []string{"fields", "create", "--label", "C", "--value-stdin"}},
		{"documents attach", "POST /api/v1/fields/118/documents", []string{"documents", "attach", "118", path}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Atomic because this handler hijacks the connection and closes it without
			// replying. A completed response would order the handler's writes before the
			// client's return; abandoning the connection orders nothing, so the count has
			// to carry its own synchronisation.
			var requests atomic.Int32
			h := newWriteHarness(t, map[string]http.HandlerFunc{
				testCase.route: func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					// Hijacked and closed without a reply: the request was received, and
					// the client will never learn what became of it.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("Hijack: %v", err)
						return
					}
					_ = conn.Close()
				},
			})
			h.app.Stdin = strings.NewReader(theSecret + "\n")

			if code := h.run(testCase.args...); code != exitcode.Unexpected {
				t.Errorf("exit = %d, want %d", code, exitcode.Unexpected)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("the server saw %d requests, want 1 — a retry here is a second write", got)
			}

			out := h.stderr.String()
			for _, want := range []string{"has not been retried", "Idempotency-Key", "dossier fields list"} {
				if !strings.Contains(out, want) {
					t.Errorf("the failure does not say %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, theSecret) {
				t.Errorf("the failure echoed the value:\n%s", out)
			}
		})
	}
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decoding the request body: %v", err)
	}
	return decoded
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
