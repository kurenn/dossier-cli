package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// newTestClient wires a client to a fake server with time under the test's control, so
// retry behaviour is asserted without any real sleeping.
func newTestClient(t *testing.T, handler http.Handler, opts ...Option) (*Client, *[]time.Duration) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	var slept []time.Duration
	base := []Option{
		WithClock(
			func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) },
			func(d time.Duration) { slept = append(slept, d) },
		),
	}

	client, err := New(server.URL, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, &slept
}

// The invariant from §1: the CLI speaks to /api/v1 and nothing else. Enforced before a
// socket opens, so it cannot be violated even transiently — the fake below fails the test
// if it is ever reached.
func TestDoRefusesPathsOutsideTheAPIPrefix(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the server was reached with path %q; the prefix check should have refused first", r.URL.Path)
	}))

	for _, path := range []string{"/d/ABC123", "/settings", "/users/sign_in", "/api/v2/fields", "/"} {
		_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: path})
		if err == nil {
			t.Errorf("Do(%q) was allowed; every path must begin with %s", path, Prefix)
			continue
		}
		if !strings.Contains(err.Error(), Prefix) {
			t.Errorf("Do(%q) failed with %q, which does not explain the prefix rule", path, err)
		}
	}
}

func TestDoSendsBearerTokenAndOmitsItWhenAnonymous(t *testing.T) {
	var seen []string
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}), WithToken("dsk_secret"))

	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"}); err != nil {
		t.Fatalf("authenticated request: %v", err)
	}
	// The schema endpoint takes no token, and sending a dead one would spend a unit of the
	// per-IP failed-auth budget for no benefit — which would make `dossier schema`, the
	// one command that works before any token exists, fail for an unrelated reason.
	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/schema", Anonymous: true}); err != nil {
		t.Fatalf("anonymous request: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, saw %d", len(seen))
	}
	if seen[0] != "Bearer dsk_secret" {
		t.Errorf("authenticated request sent Authorization %q", seen[0])
	}
	if seen[1] != "" {
		t.Errorf("anonymous request sent Authorization %q; it must send none", seen[1])
	}
}

// A cookie jar is how a client accidentally starts acting as the signed-in holder rather
// than as a bearer token, which is the boundary the whole API design rests on.
func TestClientKeepsNoCookies(t *testing.T) {
	var sawCookie bool
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			sawCookie = true
		}
		http.SetCookie(w, &http.Cookie{Name: "_dossier_session", Value: "abc"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	for range 2 {
		if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"}); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	if sawCookie {
		t.Error("the client returned a cookie the server set; it must hold no session")
	}
}

// Following a redirect with an Authorization header attached is how a bearer token ends
// up somewhere it was never minted for.
func TestClientRefusesRedirects(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/collect", http.StatusFound)
	}), WithToken("dsk_secret"))

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})
	if err == nil {
		t.Fatal("a redirect was followed")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error %q does not say a redirect was refused", err)
	}
}

func TestDoDecodesTheErrorEnvelope(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_scope","message":"This token does not have the scope this endpoint requires.","hint":"Mint a new token."}}`))
	}))

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/shares"})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *Error, got %T: %v", err, err)
	}
	if apiErr.Code != "insufficient_scope" {
		t.Errorf("Code = %q", apiErr.Code)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d", apiErr.Status)
	}
	if apiErr.ExitCode() != exitcode.ScopeMissing {
		t.Errorf("ExitCode = %d, want %d", apiErr.ExitCode(), exitcode.ScopeMissing)
	}
	// The raw body is kept so --json can pass the envelope through byte-for-byte rather
	// than re-serialising a decoded struct into a shape the API never sent.
	if !strings.Contains(string(apiErr.Body), "insufficient_scope") {
		t.Error("the raw body was not retained")
	}
}

// A failure status with a body that is not an envelope means something answered and it
// was not this API. That is exit 1, not a guess from the status class.
func TestDoTreatsANonEnvelopeBodyAsUnexpected(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *Error, got %T", err)
	}
	if apiErr.Code != "" {
		t.Errorf("Code = %q, want empty for a non-envelope body", apiErr.Code)
	}
	if apiErr.ExitCode() != exitcode.Unexpected {
		t.Errorf("ExitCode = %d, want Unexpected", apiErr.ExitCode())
	}
	if apiErr.Known() {
		t.Error("a body with no envelope was reported as a known code")
	}
}

// 429 is the only status retried blind, because it is the only one the API guarantees
// wrote nothing. The wait comes from the server's own Retry-After; the client never
// invents a backoff curve.
func TestDoHonoursRetryAfterOn429(t *testing.T) {
	var attempts int
	client, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests.","hint":"Wait."}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}), WithWaitBudget(time.Minute))

	resp, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", resp.Attempts)
	}
	if len(*slept) != 2 {
		t.Fatalf("slept %d times, want 2: %v", len(*slept), *slept)
	}
	for _, wait := range *slept {
		if wait != 12*time.Second {
			t.Errorf("slept %s, want the server's 12s", wait)
		}
	}
}

// --no-wait: a cron job with a five-minute window would rather fail now and be retried by
// its scheduler than block.
func TestDoWithNoWaitFailsImmediately(t *testing.T) {
	var attempts int
	client, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many.","hint":"Wait."}}`))
	}), WithNoWait(true))

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})

	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.ExitCode() != exitcode.TryLater {
		t.Fatalf("expected a rate_limited *Error mapping to TryLater, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("made %d attempts under --no-wait, want 1", attempts)
	}
	if len(*slept) != 0 {
		t.Errorf("slept %v under --no-wait", *slept)
	}
}

// The budget is what stops a patient client from hanging indefinitely on a limiter that
// keeps asking for more time.
func TestDoStopsWhenTheWaitBudgetIsExhausted(t *testing.T) {
	client, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many.","hint":"Wait."}}`))
	}), WithWaitBudget(60*time.Second))

	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})
	if err == nil {
		t.Fatal("expected the budget to be exhausted")
	}
	// One 45s sleep fits in the 60s budget; a second would exceed it, so the client stops.
	if len(*slept) != 1 {
		t.Errorf("slept %v, want exactly one 45s wait", *slept)
	}
}

// Nothing but 429 is retried. The API's reference is explicit about which failures may
// have written something, and a generic retry here would silently apply to endpoints with
// no idempotency guard at all.
func TestDoDoesNotRetryAnythingButRateLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a 500", http.StatusInternalServerError, `{"error":{"code":"bad_request","message":"x","hint":"y"}}`},
		{"a conflict", http.StatusConflict, `{"error":{"code":"request_in_flight","message":"x","hint":"y"}}`},
		{"a validation failure", http.StatusUnprocessableEntity, `{"error":{"code":"validation_failed","message":"x","hint":"y"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))

			_, _ = client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})
			if attempts != 1 {
				t.Errorf("made %d attempts, want 1", attempts)
			}
		})
	}
}

// §6.2, and the reason Request.Body is []byte rather than an any to be marshalled: the
// API's idempotency identity is the key plus the raw body, so a retry that re-serialised
// a struct could send different bytes under the same key and be a different request
// wearing the same name.
func TestDoSendsIdenticalBytesOnEveryRetry(t *testing.T) {
	var bodies []string
	var keys []string
	var attempts int

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		keys = append(keys, r.Header.Get("Idempotency-Key"))

		if attempts < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"x","hint":"y"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}), WithWaitBudget(time.Minute))

	payload := []byte(`{"title":"Lease","field_ids":[1,2],"expires_at":"2026-10-01T00:00:00Z"}`)
	_, err := client.Do(context.Background(), Request{
		Method:  http.MethodPost,
		Path:    Prefix + "/shares",
		Body:    payload,
		Headers: map[string]string{"Idempotency-Key": "018f3c1a-0000-7000-8000-000000000000"},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if len(bodies) != 3 {
		t.Fatalf("saw %d bodies, want 3", len(bodies))
	}
	for index, body := range bodies {
		if body != string(payload) {
			t.Errorf("attempt %d sent %q, want the original bytes %q", index+1, body, payload)
		}
		if keys[index] != "018f3c1a-0000-7000-8000-000000000000" {
			t.Errorf("attempt %d sent key %q; a retry must reuse the same key", index+1, keys[index])
		}
	}
}

// A transport failure must stay a transport failure: "the server refused you" and "the
// server was never reached" lead to different actions.
func TestDoReportsTransportFailuresSeparately(t *testing.T) {
	client, err := New("http://127.0.0.1:1", WithClock(time.Now, func(time.Duration) {}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"})
	if err == nil {
		t.Fatal("expected a transport failure")
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		t.Errorf("a connection failure was reported as an API error: %v", apiErr)
	}
}

func TestDoReportsTimeouts(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	_, err := client.Do(context.Background(), Request{
		Method:  http.MethodGet,
		Path:    Prefix + "/fields",
		Timeout: 20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not name the timeout", err)
	}
}

func TestDoPassesQueryParameters(t *testing.T) {
	var seen url.Values
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	_, err := client.Do(context.Background(), Request{
		Method: http.MethodGet,
		Path:   Prefix + "/fields",
		Query:  url.Values{"limit": []string{"1"}, "status": []string{"active"}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if seen.Get("limit") != "1" || seen.Get("status") != "active" {
		t.Errorf("query was %v", seen)
	}
}

func TestNewRejectsUnusableHosts(t *testing.T) {
	for _, host := range []string{"", "not a url", "ftp://example.com", "example.com", "/relative"} {
		if _, err := New(host); err == nil {
			t.Errorf("New(%q) was accepted", host)
		}
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	client, err := New("https://dossier.global/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Without the trim, every path would be requested as //api/v1/... — which some
	// servers treat as a protocol-relative URL.
	if client.Host() != "https://dossier.global" {
		t.Errorf("Host() = %q", client.Host())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }

	cases := []struct {
		name  string
		value string
		want  int
	}{
		{"seconds", "30", 30},
		{"zero", "0", 0},
		{"absent", "", 0},
		{"negative is clamped", "-5", 0},
		{"garbage", "soon", 0},
		{"whitespace", "  15  ", 15},
		// RFC 9110 allows an HTTP-date. Rails sends seconds, but a proxy may rewrite the
		// header, and reading 0 from a date would hammer a limiter we were asked to back
		// off from.
		{"http date in the future", "Wed, 16 Sep 2026 12:00:45 GMT", 45},
		{"http date in the past is clamped", "Wed, 16 Sep 2026 11:59:00 GMT", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// A 429 with no Retry-After still must not become a hot loop, and the floor is not an
// invented backoff curve: if the server wanted a specific wait it would have sent one.
func TestDoUsesAOneSecondFloorWhenRetryAfterIsMissing(t *testing.T) {
	var attempts int
	client, slept := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"x","hint":"y"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	if _, err := client.Do(context.Background(), Request{Method: http.MethodGet, Path: Prefix + "/fields"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Errorf("slept %v, want a single 1s floor", *slept)
	}
}

func TestResponseDecode(t *testing.T) {
	resp := &Response{Body: []byte(`{"id":88}`)}

	var target struct{ ID int }
	if err := resp.Decode(&target); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if target.ID != 88 {
		t.Errorf("ID = %d", target.ID)
	}

	bad := &Response{Body: []byte(`not json`)}
	if err := bad.Decode(&target); err == nil {
		t.Error("Decode accepted a non-JSON body")
	}
}
