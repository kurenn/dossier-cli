package api

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateFieldSendsOnlyWhatWasSet(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7,"label":"CURP","category":"identity","country":null,"sensitivity":1,"status":"active","has_document":false,"documents":[]}`)
	}))
	defer server.Close()

	client, err := New(server.URL, WithToken("dsk_x"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	field, raw, err := client.CreateField(context.Background(), CreateFieldInput{
		Label: "CURP",
		Value: "ROGV880801MDFSZL09",
	}, 0)
	if err != nil {
		t.Fatalf("CreateField: %v", err)
	}

	// The optional three are absent from the body, not sent as empty strings and a zero:
	// the server's defaults are the server's to apply, and "category": "" is a different
	// request from no category at all.
	for _, key := range []string{"category", "country", "sensitivity"} {
		if _, present := body[key]; present {
			t.Errorf("%q was sent although it was never set: %v", key, body[key])
		}
	}
	if body["label"] != "CURP" || body["value"] != "ROGV880801MDFSZL09" {
		t.Errorf("label and value did not arrive intact: %v", body)
	}
	if field.ID != 7 {
		t.Errorf("id = %d, want 7", field.ID)
	}
	if !strings.Contains(string(raw), `"id":7`) {
		t.Errorf("the raw body was not passed through: %s", raw)
	}
}

// A deliberate --sensitivity 0 must reach the server and be rejected there, rather than
// being dropped by omitempty and silently becoming the server's default of 1.
func TestCreateFieldSendsADeliberateZeroSensitivity(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	zero := 0
	if _, _, err := client.CreateField(context.Background(), CreateFieldInput{
		Label: "X", Value: "y", Sensitivity: &zero,
	}, 0); err != nil {
		t.Fatalf("CreateField: %v", err)
	}

	value, present := body["sensitivity"]
	if !present {
		t.Fatal("sensitivity was dropped; the server would default it to 1 and the holder would never know")
	}
	if value != float64(0) {
		t.Errorf("sensitivity = %v, want 0", value)
	}
}

func TestAttachDocumentDeclaresTheTypeOnThePart(t *testing.T) {
	var (
		partName        string
		partFilename    string
		partContentType string
		partBody        []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("Content-Type = %q, want multipart", r.Header.Get("Content-Type"))
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			t.Errorf("reading the part: %v", err)
			return
		}
		partName = part.FormName()
		partFilename = part.FileName()
		partContentType = part.Header.Get("Content-Type")
		partBody, _ = io.ReadAll(part)

		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"document":{"id":43,"field_id":118,"byte_size":7,"content_type":"application/pdf"}}`)
	}))
	defer server.Close()

	client, _ := New(server.URL, WithToken("dsk_x"))
	document, raw, err := client.AttachDocument(context.Background(), 118, Upload{
		Filename:    "passport.pdf",
		ContentType: "application/pdf",
		Content:     []byte("%PDF-1."),
	}, 0)
	if err != nil {
		t.Fatalf("AttachDocument: %v", err)
	}

	if partName != "file" {
		t.Errorf("part name = %q, want \"file\"", partName)
	}
	if partFilename != "passport.pdf" {
		t.Errorf("filename = %q", partFilename)
	}
	// The whole reason the part is built by hand: CreateFormFile would declare
	// application/octet-stream here, and the API allow-lists on the declaration.
	if partContentType != "application/pdf" {
		t.Errorf("part Content-Type = %q, want application/pdf — the API rejects octet-stream", partContentType)
	}
	if string(partBody) != "%PDF-1." {
		t.Errorf("part body = %q", partBody)
	}
	if document.ID != 43 || document.FieldID != 118 {
		t.Errorf("document = %+v", document)
	}
	if !strings.Contains(string(raw), `"document"`) {
		t.Errorf("raw body not passed through: %s", raw)
	}
}

func TestAttachDocumentEscapesAQuoteInTheFilename(t *testing.T) {
	var filename string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Errorf("reading the part: %v", err)
			return
		}
		filename = part.FileName()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"document":{"id":1}}`)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	if _, _, err := client.AttachDocument(context.Background(), 1, Upload{
		Filename:    `a"b.pdf`,
		ContentType: "application/pdf",
		Content:     []byte("x"),
	}, 0); err != nil {
		t.Fatalf("AttachDocument: %v", err)
	}

	if filename != `a"b.pdf` {
		t.Errorf("filename = %q, want %q — an unescaped quote ends the header early", filename, `a"b.pdf`)
	}
}

// The M2 done-when, and the most important test in this file.
//
// Neither write endpoint takes an Idempotency-Key. A retry after the body has been sent
// is therefore a second write: for a field it is a duplicate-label 422 at best, and for
// a document it silently attaches a second copy. So the client must send exactly once
// and report the ambiguity, which is what these two assert by counting requests against
// a server that never answers.
func TestWritesAreSentExactlyOnceWhenTheServerNeverAnswers(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
	}{
		{
			name: "CreateField",
			call: func(c *Client) error {
				_, _, err := c.CreateField(context.Background(), CreateFieldInput{Label: "X", Value: "y"}, 100*time.Millisecond)
				return err
			},
		},
		{
			name: "AttachDocument",
			call: func(c *Client) error {
				_, _, err := c.AttachDocument(context.Background(), 1, Upload{
					Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("x"),
				}, 100*time.Millisecond)
				return err
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			received := make(chan struct{}, 8)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Drained first, so the body really has been sent: this is the ambiguous
				// case — the server received the request and the client will never learn
				// what it did with it.
				_, _ = io.Copy(io.Discard, r.Body)
				received <- struct{}{}
				<-release
			}))
			defer func() {
				close(release)
				server.Close()
			}()

			client, _ := New(server.URL, WithToken("dsk_x"))
			err := testCase.call(client)
			if err == nil {
				t.Fatal("a request that timed out returned no error")
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Errorf("error = %q, want a timeout", err)
			}

			// Give a retry, if there were one, time to arrive.
			time.Sleep(200 * time.Millisecond)
			if got := len(received); got != 1 {
				t.Errorf("the server saw %d requests, want exactly 1 — a retry here is a second write", got)
			}
		})
	}
}

// A 429 is the one safe repeat: the limiter refuses before the handler runs, so nothing
// was written and the same bytes can be sent again.
func TestWritesRetryARateLimitBecauseNothingWasWritten(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"Too many requests."}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":9,"label":"CURP"}`)
	}))
	defer server.Close()

	var slept time.Duration
	client, _ := New(server.URL,
		WithClock(time.Now, func(d time.Duration) { slept += d }),
	)

	field, _, err := client.CreateField(context.Background(), CreateFieldInput{Label: "CURP", Value: "v"}, 0)
	if err != nil {
		t.Fatalf("CreateField: %v", err)
	}
	if field.ID != 9 {
		t.Errorf("id = %d, want 9", field.ID)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if slept != time.Second {
		t.Errorf("slept %s, want the 1s the server asked for", slept)
	}
}

func TestMaxFileBytesReadsTheSchemasNumber(t *testing.T) {
	var schema Schema
	if err := json.Unmarshal([]byte(`{"limits":{"uploads":{"max_file_bytes":26214400}}}`), &schema); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	size, published := schema.MaxFileBytes()
	if !published || size != 26214400 {
		t.Errorf("MaxFileBytes() = %d, %v; want 26214400, true", size, published)
	}

	// An absent cap says so, rather than returning a zero that would refuse every file.
	var empty Schema
	if _, published := empty.MaxFileBytes(); published {
		t.Error("a schema with no uploads block claimed to publish a cap")
	}
}
