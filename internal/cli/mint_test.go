package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// mintFixture is a 201 shaped like the real one, including the raw PIN — the only response
// in the API that carries one.
const mintFixture = `{"batch_id":"b6f2b3f0-9e7a-4c8e-9d21-2f6a8b3c4d5e","shares":[{"id":42,"token":"K7M2P9QRX","url":"https://dossier.global/d/k7m2p9qrx","pin":"480 217","pin_shown_once":true,"recipient":{"label":"Marisol Vega","email":"marisol@example.com"},"state":"live","expires_at":"2026-09-21T09:00:00Z","field_count":1,"email_queued":true}]}`

// newMintHarness seeds a profile and neutralises every wait, so the retry table's three
// minutes of bounded patience run instantly and the durations it chose can still be
// asserted.
func newMintHarness(t *testing.T, routes map[string]http.HandlerFunc) (*harness, *[]time.Duration) {
	t.Helper()

	h := newHarness(t)
	if _, taken := routes["/api/v1/schema"]; !taken {
		routes["/api/v1/schema"] = jsonResponse(http.StatusOK, schemaFixture)
	}
	server := fakeAPI(t, routes)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: "dsk_mint"})
	h.seedDefaultProfile("default")

	var slept []time.Duration
	h.app.Sleep = func(d time.Duration) { slept = append(slept, d) }
	return h, &slept
}

func mintArgs(extra ...string) []string {
	return append([]string{
		"shares", "mint",
		"--field", "1",
		"--to", "Marisol Vega <marisol@example.com>",
		"--title", "Lease application",
		"--yes",
	}, extra...)
}

// ledgerKeys is every key with an entry on disk. The count is the M3 done-when's "one
// key", and the emptiness is how "resolved" is asserted.
func (h *harness) ledgerKeys() []string {
	h.t.Helper()

	entries, err := h.paths.ListMints()
	if err != nil {
		h.t.Fatalf("ListMints: %v", err)
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys
}

// The first M3 done-when: a non-TTY mint with neither expiry flag exits 2 before any
// request. The fake has no /shares route, so a request would fail the test — which is how
// "before any request" is asserted rather than assumed.
func TestMintRefusesWithoutAnExpiryAndSendsNothing(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if code := h.run("shares", "mint", "--field", "1", "--to", "Marisol Vega"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}

	// The API's own copy for expiry_required, both halves, read out of the schema rather
	// than written in the CLI.
	for _, want := range []string{
		`Expiry is required. Pass "expires_at" explicitly.`,
		"explicit null for the labelled no-expiry exception",
		"--no-expiry",
		"Nothing was sent.",
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, h.stderr.String())
		}
	}
	if keys := h.ledgerKeys(); len(keys) != 0 {
		t.Errorf("a refused mint left %d ledger entries", len(keys))
	}
}

// --yes skips the confirmation, not the expiry requirement. Nothing skips that.
func TestMintYesDoesNotSkipTheExpiryRequirement(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if code := h.run("shares", "mint", "--field", "1", "--to", "Marisol Vega", "--yes"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d — --yes must not construct a deadline", code, exitcode.Usage)
	}
}

// The second done-when: --expires 7d sends now + P7D, and prints the instant rather than
// the arithmetic.
func TestMintSendsTheComputedDeadlineAndPrintsIt(t *testing.T) {
	var sent json.RawMessage
	var key string

	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			body := readAll(t, r)
			sent = body
			key = r.Header.Get(api.IdempotencyHeader)
			jsonResponse(http.StatusCreated, mintFixture)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "7d")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	var decoded struct {
		ExpiresAt *string `json:"expires_at"`
		FieldIDs  []int   `json:"field_ids"`
		Title     string  `json:"title"`
	}
	if err := json.Unmarshal(sent, &decoded); err != nil {
		t.Fatalf("the body was not JSON: %v", err)
	}
	want := fixedNow.AddDate(0, 0, 7).UTC().Format(time.RFC3339)
	if decoded.ExpiresAt == nil || *decoded.ExpiresAt != want {
		t.Errorf("expires_at = %v, want %q", decoded.ExpiresAt, want)
	}

	// The key is mandatory on this endpoint and generated per run.
	if !store.ValidKey(key) {
		t.Errorf("Idempotency-Key = %q, which is not a usable key", key)
	}

	// A 201 resolves the key, so nothing is left to resume.
	if keys := h.ledgerKeys(); len(keys) != 0 {
		t.Errorf("a confirmed mint left the ledger entries %v", keys)
	}
}

// The handover block of §6.4, byte for byte. The golden is the whole point: this is the
// one screen in the product that shows a secret exactly once, and a paraphrase of any part
// of it is a defect.
func TestMintRendersTheHandoverBlock(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusCreated, mintFixture),
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	assertGolden(t, "mint-handover.txt", h.stdout.String())
	assertGolden(t, "mint-handover.stderr.txt", h.stderr.String())
}

// Without an address, the CLI's output is the only copy of the PIN — and it has to say so,
// because the alternative reading ("they were emailed it") is the one that gets the PIN
// forwarded through the same channel as the link.
func TestMintSaysWhoHasThePIN(t *testing.T) {
	cases := []struct {
		name        string
		emailQueued bool
		wantBlock   string
		wantProse   string
		notProse    string
	}{
		{
			name:        "an address was on file",
			emailQueued: true,
			wantBlock:   "EMAILED    address on file",
			wantProse:   "has been sent the link and this PIN. Do not send the PIN again by another channel.",
			notProse:    "only copy",
		},
		{
			name:        "no address",
			emailQueued: false,
			wantBlock:   "EMAILED    none",
			wantProse:   "This is the only copy of the PIN. Hand it to the recipient yourself, by a different channel from the link.",
			notProse:    "has been sent",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := strings.Replace(mintFixture,
				`"email_queued":true`,
				fmt.Sprintf(`"email_queued":%t`, testCase.emailQueued), 1)
			if !testCase.emailQueued {
				body = strings.Replace(body, `"email":"marisol@example.com"`, `"email":null`, 1)
			}

			h, _ := newMintHarness(t, map[string]http.HandlerFunc{
				"/api/v1/shares": jsonResponse(http.StatusCreated, body),
			})
			if code := h.run(mintArgs("--expires", "P7D")...); code != 0 {
				t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
			}

			if !strings.Contains(h.stdout.String(), testCase.wantBlock) {
				t.Errorf("block does not carry %q:\n%s", testCase.wantBlock, h.stdout.String())
			}
			if !strings.Contains(h.stderr.String(), testCase.wantProse) {
				t.Errorf("prose does not carry %q:\n%s", testCase.wantProse, h.stderr.String())
			}
			if strings.Contains(h.stderr.String(), testCase.notProse) {
				t.Errorf("prose carries the other case's %q:\n%s", testCase.notProse, h.stderr.String())
			}

			// Never yes/no. email_queued is whether an address existed, and the API has no
			// delivery signal to report — so a row reading "yes" would tell a holder a mail
			// arrived that the server cannot see. Matched as whole lines, because "no" is a
			// prefix of the legitimate "none".
			for _, line := range strings.Split(h.stdout.String(), "\n") {
				if trimmed := strings.TrimSpace(line); trimmed == "EMAILED    yes" || trimmed == "EMAILED    no" {
					t.Errorf("the block claims a delivery signal the API does not have: %q", trimmed)
				}
			}
		})
	}
}

// The third done-when: a 429 then a 201 produces one share and one key. The 429 never
// claims the key, so this is the retry that is genuinely free — and it must still be the
// same key, or it would be a second mint.
func TestMintRetriesARateLimitUnderTheSameKey(t *testing.T) {
	var keys []string
	var bodies []string
	var attempts atomic.Int32

	h, slept := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			keys = append(keys, r.Header.Get(api.IdempotencyHeader))
			bodies = append(bodies, string(readAll(t, r)))

			if attempts.Add(1) == 1 {
				w.Header().Set("Retry-After", "60")
				jsonResponse(http.StatusTooManyRequests,
					`{"error":{"code":"rate_limited","message":"Too many requests.","hint":"Check the \"Retry-After\" header (seconds) and back off."}}`)(w, r)
				return
			}
			jsonResponse(http.StatusCreated, mintFixture)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("%d requests, want 2", got)
	}
	if keys[0] != keys[1] {
		t.Errorf("the retry used a new key (%s then %s), which would be a second mint", keys[0], keys[1])
	}
	if bodies[0] != bodies[1] {
		t.Error("the retry sent different bytes, which the API answers with idempotency_key_reused")
	}
	if len(*slept) != 1 || (*slept)[0] != 60*time.Second {
		t.Errorf("waits = %v, want one 60s wait honouring Retry-After", *slept)
	}
	if keys := h.ledgerKeys(); len(keys) != 0 {
		t.Errorf("the resolved mint left %v behind", keys)
	}
	if !strings.Contains(h.stderr.String(), "Rate limited. Waiting 1m") {
		t.Errorf("the CLI waited without saying so, which reads as a hang:\n%s", h.stderr.String())
	}
}

// --no-wait exits 8 immediately and keeps the ledger. Nothing was claimed server-side, so
// a later --resume is a first attempt under a key that happens to already exist locally.
func TestMintNoWaitExitsTryLaterAndKeepsTheKey(t *testing.T) {
	h, slept := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			jsonResponse(http.StatusTooManyRequests,
				`{"error":{"code":"rate_limited","message":"Too many requests."}}`)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D", "--no-wait")...); code != exitcode.TryLater {
		t.Fatalf("exit = %d, want %d", code, exitcode.TryLater)
	}
	if len(*slept) != 0 {
		t.Errorf("--no-wait slept %v", *slept)
	}

	keys := h.ledgerKeys()
	if len(keys) != 1 {
		t.Fatalf("%d ledger entries, want 1 — the key must be resumable", len(keys))
	}
	if !strings.Contains(h.stderr.String(), "--resume "+keys[0]) {
		t.Errorf("the refusal does not name the key to resume:\n%s", h.stderr.String())
	}
}

// The fourth done-when: a connection dropped after the body was received leaves a ledger
// entry, and --resume completes it to a byte-identical 201.
//
// This is the case the whole mechanism exists for. The CLI cannot know whether the mint
// landed, and the only safe move is to ask again under the same key — which the server
// answers from its own record.
func TestMintResumeCompletesAnUnknownOutcome(t *testing.T) {
	// Answering is flipped on before the resume rather than counting attempts, because
	// net/http replays a POST carrying an Idempotency-Key when a *reused* connection dies
	// — see TestNetHTTPReplaysOnlyTheKeyedPost — so how many times the handler is reached
	// during the first run is the transport's business, not this test's.
	var answering atomic.Bool

	var keys []string
	var bodies []string

	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			keys = append(keys, r.Header.Get(api.IdempotencyHeader))
			bodies = append(bodies, string(readAll(t, r)))

			if !answering.Load() {
				// The body was read, so the request reached the server; then the
				// connection dies before any status. Indistinguishable, from the client,
				// from a mint that succeeded and lost its response.
				hijackAndClose(t, w)
				return
			}
			jsonResponse(http.StatusCreated, mintFixture)(w, r)
		},
	})

	code := h.run(mintArgs("--expires", "P7D")...)
	if code != exitcode.Unexpected {
		t.Fatalf("exit = %d, want %d", code, exitcode.Unexpected)
	}

	keysOnDisk := h.ledgerKeys()
	if len(keysOnDisk) != 1 {
		t.Fatalf("%d ledger entries, want 1", len(keysOnDisk))
	}
	key := keysOnDisk[0]

	// The advice must name the exact command, because at this moment the holder does not
	// know whether they have released a dossier.
	for _, want := range []string{
		"may have reached Dossier",
		"dossier shares mint --resume " + key,
		"answers from the first one",
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the advice does not carry %q:\n%s", want, h.stderr.String())
		}
	}

	h.reset()
	h.app.Sleep = func(time.Duration) {}
	answering.Store(true)

	if code := h.run("shares", "mint", "--resume", key); code != 0 {
		t.Fatalf("resume exit = %d:\n%s", code, h.stderr.String())
	}

	// Every attempt, transport replays included, under one key with one body. That is the
	// property the whole mechanism rests on.
	for i := range keys {
		if keys[i] != keys[0] {
			t.Errorf("attempt %d used a different key: %v", i+1, keys)
		}
		if bodies[i] != bodies[0] {
			t.Errorf("attempt %d sent different bytes:\n%q\n%q", i+1, bodies[i], bodies[0])
		}
	}
	if got := h.ledgerKeys(); len(got) != 0 {
		t.Errorf("the resumed mint left %v behind", got)
	}
	// The title is not in the 201, so a resumed block can only have got it from the
	// stored body — which is the one record of it on this side.
	if !strings.Contains(h.stdout.String(), "TITLE      Lease application") {
		t.Errorf("the resumed block lost its title:\n%s", h.stdout.String())
	}
}

// The fifth done-when: a 500 then a 201 on the same key mints one share and exits 0.
//
// One retry, because that retry's answer is the only thing that distinguishes "it had not
// minted" from "it may already have". The API asks for it by name.
func TestMintRetriesOnceAfterAServerErrorAndSucceeds(t *testing.T) {
	var attempts atomic.Int32
	var keys []string

	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			keys = append(keys, r.Header.Get(api.IdempotencyHeader))
			if attempts.Add(1) == 1 {
				jsonResponse(http.StatusInternalServerError, `{"error":{"code":"server_error","message":"Something went wrong."}}`)(w, r)
				return
			}
			jsonResponse(http.StatusCreated, mintFixture)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("%d requests, want exactly 2", got)
	}
	if keys[0] != keys[1] {
		t.Errorf("the retry used a new key, which is a second mint: %v", keys)
	}
	if got := h.ledgerKeys(); len(got) != 0 {
		t.Errorf("a confirmed mint left %v behind", got)
	}
	if !strings.Contains(h.stderr.String(), "does not say whether the dossier was minted") {
		t.Errorf("the CLI retried without explaining why:\n%s", h.stderr.String())
	}
}

// The sixth done-when: a 500 then a 409 request_in_flight exits 11 without a third
// request.
//
// This is the claim that never resolves itself — the batch was written but the response
// could not be stored — and a backoff loop here would spin until the 24 h TTL and then
// mint again. The 5xx recorded in the ledger is the only thing that tells this apart from
// an ordinary concurrent claim.
func TestMintExitsUnconfirmedWhenAnInFlightFollowsAServerError(t *testing.T) {
	var attempts atomic.Int32

	h, slept := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) == 1 {
				jsonResponse(http.StatusInternalServerError, `{"error":{"code":"server_error","message":"Something went wrong."}}`)(w, r)
				return
			}
			jsonResponse(http.StatusConflict,
				`{"error":{"code":"request_in_flight","message":"A request with this Idempotency-Key is still being processed.","hint":"Wait for the first request to finish, then retry with the same key."}}`)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != exitcode.MintUnconfirmed {
		t.Fatalf("exit = %d, want %d", code, exitcode.MintUnconfirmed)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("%d requests, want exactly 2 — the third would be the spin this avoids", got)
	}
	if len(*slept) != 0 {
		t.Errorf("the unresolvable claim was backed off %v, which would spin for 24h", *slept)
	}

	for _, want := range []string{
		"cannot confirm it",
		"may already exist",
		"dossier shares list",
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the reconcile advice does not carry %q:\n%s", want, h.stderr.String())
		}
	}
	if got := h.ledgerKeys(); len(got) != 1 {
		t.Errorf("%d ledger entries, want 1 kept", len(got))
	}
}

// A 409 with no 5xx recorded is an ordinary concurrent claim: backed off 1s, 2s, 4s, then
// exit 8 with the ledger kept. The difference from the test above is one field in the
// ledger, and it is the difference between waiting and giving up.
func TestMintBacksOffAnOrdinaryInFlightClaim(t *testing.T) {
	var attempts atomic.Int32

	h, slept := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			jsonResponse(http.StatusConflict,
				`{"error":{"code":"request_in_flight","message":"A request with this Idempotency-Key is still being processed."}}`)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != exitcode.TryLater {
		t.Fatalf("exit = %d, want %d", code, exitcode.TryLater)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("%d requests, want 4 — one attempt and three retries", got)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("waits = %v, want %v", *slept, want)
	}
	for i, d := range want {
		if (*slept)[i] != d {
			t.Errorf("wait %d = %s, want %s", i+1, (*slept)[i], d)
		}
	}
}

// A 422 is the server having decided. Nothing was minted, a corrected body needs a new
// key, and the entry is removed — leaving it would offer a resume that could only
// reproduce the same rejection.
func TestMintDeletesTheLedgerAfterAValidationFailure(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusUnprocessableEntity,
			`{"error":{"code":"no_fields","message":"Select at least one field to share.","hint":"field_ids must name active fields this token's account owns."}}`),
	})

	want, _ := exitcode.FromErrorCode("no_fields")
	if code := h.run(mintArgs("--expires", "P7D")...); code != want {
		t.Fatalf("exit = %d, want %d", code, want)
	}
	if got := h.ledgerKeys(); len(got) != 0 {
		t.Errorf("a rejected mint left %v to resume, which could only be rejected again", got)
	}
	if !strings.Contains(h.stderr.String(), "Select at least one field to share.") {
		t.Errorf("the API's own message was not rendered:\n%s", h.stderr.String())
	}
}

// insufficient_scope claims nothing, so there is nothing to resume either.
func TestMintDeletesTheLedgerAfterAScopeRefusal(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusForbidden,
			`{"error":{"code":"insufficient_scope","message":"This token does not have the scope this endpoint requires."}}`),
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != exitcode.ScopeMissing {
		t.Fatalf("exit = %d, want %d", code, exitcode.ScopeMissing)
	}
	if got := h.ledgerKeys(); len(got) != 0 {
		t.Errorf("a scope refusal left %v behind", got)
	}
}

// idempotency_key_reused is never retried, and the message says what realistically caused
// it: the CLI records the bytes it sends, so the only way to reuse a key with a different
// body is to have supplied the key by hand.
func TestMintDoesNotRetryAReusedKey(t *testing.T) {
	var attempts atomic.Int32

	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			jsonResponse(http.StatusConflict,
				`{"error":{"code":"idempotency_key_reused","message":"This Idempotency-Key was already used with a different request body.","hint":"Use a new key for a genuinely new request."}}`)(w, r)
		},
	})

	want, _ := exitcode.FromErrorCode("idempotency_key_reused")
	if code := h.run(mintArgs("--expires", "P7D", "--idempotency-key", "handpicked-key")...); code != want {
		t.Fatalf("exit = %d, want %d", code, want)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("%d requests, want 1 — retrying this can only fail again", got)
	}
	if !strings.Contains(h.stderr.String(), "--idempotency-key") {
		t.Errorf("the message does not name the one way this happens:\n%s", h.stderr.String())
	}
}

// net/http replays a POST whose connection died — but only one carrying an
// Idempotency-Key.
//
// This is not our code, and it is load-bearing for two milestones, so it is pinned here
// rather than assumed. `Request.isReplayable` treats a POST as safe to resend on a reused
// connection that closed before any response if and only if the request has an
// `Idempotency-Key` (or `X-Idempotency-Key`) header — the same signal, for the same
// reason, that this API uses.
//
// The consequences fall out neatly, which is why it is worth knowing rather than merely
// working around:
//
//   - `shares mint` sends that header, so a dropped connection may be replayed under the
//     hood. That replay carries the same key and the same bytes, so it is exactly the
//     retry §6.3 prescribes and cannot mint twice.
//   - `fields create` and `documents attach` send no such header, so they are never
//     replayed. M2's exactly-once guarantee for two endpoints with no idempotency guard
//     at all is therefore structural rather than a hope about connection reuse.
//
// If a future Go relaxed this, the second bullet would silently become false — and a
// retried `fields create` is a second field. Hence the test.
func TestNetHTTPReplaysOnlyTheKeyedPost(t *testing.T) {
	cases := []struct {
		name        string
		header      string
		wantReplays bool
	}{
		{"with an Idempotency-Key", api.IdempotencyHeader, true},
		{"without one", "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var attempts atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/warm" {
					// Answered normally, so the connection goes back to the pool as idle.
					// Without this the POST opens a fresh connection, which net/http never
					// replays regardless of the header.
					w.WriteHeader(http.StatusOK)
					return
				}
				attempts.Add(1)
				_ = readAll(t, r)
				hijackAndClose(t, w)
			}))
			defer server.Close()

			client := server.Client()

			warm, err := client.Get(server.URL + "/warm")
			if err != nil {
				t.Fatalf("warming the connection: %v", err)
			}
			_ = readAll(t, &http.Request{Body: warm.Body})
			warm.Body.Close()

			request, err := http.NewRequest(http.MethodPost, server.URL+"/post", strings.NewReader(`{"a":1}`))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if testCase.header != "" {
				request.Header.Set(testCase.header, "a-key")
			}

			response, err := client.Do(request)
			if err == nil {
				response.Body.Close()
				t.Fatal("the hijacked connection produced a response")
			}

			got := attempts.Load()
			if testCase.wantReplays && got < 2 {
				t.Errorf("%d attempts; Go no longer replays a keyed POST, which this CLI's "+
					"mint retry reasoning assumes", got)
			}
			if !testCase.wantReplays && got != 1 {
				t.Errorf("%d attempts, want 1: Go replayed a POST with no idempotency key, "+
					"which would make a retried `fields create` a second field", got)
			}
		})
	}
}

// A ledger entry past the server's 24 h claim TTL cannot be resumed. Past that line the
// same key is a new mint rather than a replay, which is the double-mint the ledger exists
// to prevent — so this refuses rather than risk it.
func TestMintResumeRefusesAStaleEntry(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	body, err := api.BuildMintBody(api.MintInput{FieldIDs: []int{1}})
	if err != nil {
		t.Fatalf("BuildMintBody: %v", err)
	}
	created := fixedNow.Add(-25 * time.Hour)
	if err := h.paths.SaveMint(&store.MintEntry{
		Key:       "stale-key",
		Body:      body,
		Digest:    api.BodyDigest(body),
		CreatedAt: created,
	}); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	if code := h.run("shares", "mint", "--resume", "stale-key"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
	for _, want := range []string{"25h old", "24 hours", "dossier shares list"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, h.stderr.String())
		}
	}
}

// A ledger file whose body no longer matches its digest is refused rather than resent.
// Sending edited bytes under a spent key would earn idempotency_key_reused, which is
// permanent for that key — so the checksum is what stops a corrupt entry from making the
// situation worse.
func TestMintResumeRefusesATamperedEntry(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if err := h.paths.SaveMint(&store.MintEntry{
		Key:       "tampered-key",
		Body:      []byte(`{"field_ids":[1],"expires_at":null}`),
		Digest:    "not-the-digest-of-that-body",
		CreatedAt: fixedNow,
	}); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	if code := h.run("shares", "mint", "--resume", "tampered-key"); code != exitcode.Unexpected {
		t.Fatalf("exit = %d, want %d", code, exitcode.Unexpected)
	}
	if !strings.Contains(h.stderr.String(), "does not match its own checksum") {
		t.Errorf("the refusal does not say why:\n%s", h.stderr.String())
	}
}

func TestMintResumeWithNoSuchKeySaysSo(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if code := h.run("shares", "mint", "--resume", "never-existed"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(h.stderr.String(), "nothing outstanding to finish") {
		t.Errorf("the message does not explain what an absent key means:\n%s", h.stderr.String())
	}
}

// Resuming against a different host is refused. A claim belongs to the vault it was made
// on, so the same key elsewhere is a new mint — and would release a second dossier.
func TestMintResumeRefusesADifferentHost(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	body, _ := api.BuildMintBody(api.MintInput{FieldIDs: []int{1}})
	if err := h.paths.SaveMint(&store.MintEntry{
		Key:         "elsewhere-key",
		Body:        body,
		Digest:      api.BodyDigest(body),
		Host:        "https://another-vault.example",
		ProfileName: "other",
		CreatedAt:   fixedNow,
	}); err != nil {
		t.Fatalf("SaveMint: %v", err)
	}

	if code := h.run("shares", "mint", "--resume", "elsewhere-key"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(h.stderr.String(), "another-vault.example") {
		t.Errorf("the refusal does not name the host it was claimed on:\n%s", h.stderr.String())
	}
}

// The ledger entry is written before the request, not after.
//
// Its entire value is in the window between "sent" and "answered". An entry created from
// the response would be missing for exactly the failures it exists to recover from.
func TestMintWritesTheLedgerBeforeSending(t *testing.T) {
	var sawEntry bool

	// Declared before the routes so the handler can close over it; the handler only runs
	// once h.run is called, by which point it is assigned.
	var h *harness

	h, _ = newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			// Read from inside the handler: at this moment the CLI has sent and has no
			// answer, which is the state the ledger must already cover.
			key := r.Header.Get(api.IdempotencyHeader)
			if _, err := os.Stat(filepath.Join(h.paths.StateDir, "mints", key+".json")); err == nil {
				sawEntry = true
			}
			jsonResponse(http.StatusCreated, mintFixture)(w, r)
		},
	})

	if code := h.run(mintArgs("--expires", "P7D")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}
	if !sawEntry {
		t.Error("the ledger entry did not exist while the request was in flight")
	}
}

// --json passes the body through, PIN included. §6.4 makes that the holder's decision when
// they ask for --json, and the CLI does not redact a response the API chose to return.
func TestMintJSONIsTheServersBytes(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusCreated, mintFixture),
	})

	if code := h.run(append(mintArgs("--expires", "P7D"), "--json")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}
	if got := strings.TrimSpace(h.stdout.String()); got != mintFixture {
		t.Errorf("stdout is not the server's bytes:\n got %s\nwant %s", got, mintFixture)
	}
}

// Every recipient becomes its own dossier with its own PIN, and the block is per share.
func TestMintRendersOneBlockPerRecipient(t *testing.T) {
	const batch = `{"batch_id":"b6f2b3f0-9e7a-4c8e-9d21-2f6a8b3c4d5e","shares":[
    {"id":42,"token":"K7M2P9QRX","url":"https://dossier.global/d/k7m2p9qrx","pin":"480 217","pin_shown_once":true,"recipient":{"label":"Marisol Vega","email":"marisol@example.com"},"state":"live","expires_at":"2026-09-21T09:00:00Z","field_count":1,"email_queued":true},
    {"id":43,"token":"TQ4R8WLPZ","url":"https://dossier.global/d/tq4r8wlpz","pin":"913 044","pin_shown_once":true,"recipient":{"label":"Tomas Ruiz","email":null},"state":"live","expires_at":"2026-09-21T09:00:00Z","field_count":1,"email_queued":false}
  ]}`

	var sent []byte
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			sent = readAll(t, r)
			jsonResponse(http.StatusCreated, batch)(w, r)
		},
	})

	code := h.run("shares", "mint",
		"--field", "1",
		"--to", "Marisol Vega <marisol@example.com>",
		"--to", "Tomas Ruiz",
		"--expires", "P7D", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	// Both recipients in the order given: the first is primary, the rest additional.
	var decoded struct {
		Recipients []api.Recipient `json:"recipients"`
	}
	if err := json.Unmarshal(sent, &decoded); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(decoded.Recipients) != 2 ||
		decoded.Recipients[0].Email != "marisol@example.com" ||
		decoded.Recipients[1].Label != "Tomas Ruiz" ||
		decoded.Recipients[1].Email != "" {
		t.Errorf("recipients = %+v", decoded.Recipients)
	}

	for _, pin := range []string{"480 217", "913 044"} {
		if !strings.Contains(h.stdout.String(), pin) {
			t.Errorf("the block for one recipient is missing its own PIN %q:\n%s", pin, h.stdout.String())
		}
	}
	if !strings.Contains(h.stderr.String(), "2 dossiers, one batch") {
		t.Errorf("the batch is not explained:\n%s", h.stderr.String())
	}
}

// The picker's shape is the product argument, not decoration.
//
// Product rule 2 says expiry is required and not a setting, and that "no expiry" is the
// one labelled exception. So the four presets are numbered together, a rule separates
// them, and the exception sits alone below it carrying the API's own note. A picker that
// listed five equal options would contradict the rule while appearing to implement it —
// which is why the separator and the note are asserted, not just the choices.
func TestMintPickerSetsTheExceptionApart(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusCreated, mintFixture),
	})
	h.app.IsInteractive = func() bool { return true }
	h.stdinString("2\n")

	if code := h.run("shares", "mint", "--field", "1", "--to", "Marisol Vega", "--yes"); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}

	assertGolden(t, "mint-picker.txt", h.stderr.String())
}

// Each choice resolves to the preset it names, and the last one is the exception.
func TestMintPickerChoicesResolveToTheSchemasPresets(t *testing.T) {
	cases := []struct {
		answer string
		want   string
	}{
		{"1", fixedNow.Add(24 * time.Hour).UTC().Format(time.RFC3339)},
		{"2", fixedNow.AddDate(0, 0, 7).UTC().Format(time.RFC3339)},
		{"3", fixedNow.AddDate(0, 0, 30).UTC().Format(time.RFC3339)},
		{"4", fixedNow.AddDate(0, 0, 90).UTC().Format(time.RFC3339)},
		{"5", "null"},
	}

	for _, testCase := range cases {
		t.Run("choice "+testCase.answer, func(t *testing.T) {
			var sent []byte
			h, _ := newMintHarness(t, map[string]http.HandlerFunc{
				"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
					sent = readAll(t, r)
					jsonResponse(http.StatusCreated, mintFixture)(w, r)
				},
			})
			h.app.IsInteractive = func() bool { return true }
			h.stdinString(testCase.answer + "\n")

			if code := h.run("shares", "mint", "--field", "1", "--to", "Marisol Vega", "--yes"); code != 0 {
				t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
			}

			want := `"expires_at":` + testCase.want
			if testCase.want != "null" {
				want = `"expires_at":"` + testCase.want + `"`
			}
			if !strings.Contains(string(sent), want) {
				t.Errorf("body = %s, want %s", sent, want)
			}
		})
	}
}

func TestMintPickerRefusesAChoiceOutsideTheList(t *testing.T) {
	for _, answer := range []string{"0", "6", "", "seven", "-1"} {
		h, _ := newMintHarness(t, map[string]http.HandlerFunc{})
		h.app.IsInteractive = func() bool { return true }
		h.stdinString(answer + "\n")

		code := h.run("shares", "mint", "--field", "1", "--to", "Marisol Vega", "--yes")
		if code != exitcode.Usage {
			t.Errorf("answer %q exited %d, want %d", answer, code, exitcode.Usage)
		}
		if !strings.Contains(h.stderr.String(), "Nothing was minted.") {
			t.Errorf("answer %q did not say nothing happened:\n%s", answer, h.stderr.String())
		}
	}
}

// The confirmation shows what is about to be released, and declining mints nothing.
func TestMintConfirmationCanBeDeclined(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})
	h.app.IsInteractive = func() bool { return true }
	h.stdinString("n\n")

	code := h.run("shares", "mint",
		"--field", "1", "--field", "2",
		"--to", "Marisol Vega <marisol@example.com>",
		"--title", "Lease application",
		"--expires", "P7D")
	if code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(h.stderr.String(), "Nothing was minted.") {
		t.Errorf("declining did not say so:\n%s", h.stderr.String())
	}
	if got := h.ledgerKeys(); len(got) != 0 {
		t.Errorf("a declined mint claimed the key %v", got)
	}

	// What it showed before asking: the deadline in particular, because that is the thing
	// the holder is confirming.
	for _, want := range []string{
		"TITLE       Lease application",
		"FIELDS      2",
		"RECIPIENTS  Marisol Vega",
		"EXPIRES     2026-09-23T12:00:00Z",
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the confirmation does not show %q:\n%s", want, h.stderr.String())
		}
	}
}

func TestParseRecipients(t *testing.T) {
	cases := []struct {
		in    string
		label string
		email string
	}{
		{"Marisol Vega <marisol@example.com>", "Marisol Vega", "marisol@example.com"},
		{"  Marisol Vega  <marisol@example.com>  ", "Marisol Vega", "marisol@example.com"},
		{"Marisol Vega", "Marisol Vega", ""},
		{"marisol@example.com", "", "marisol@example.com"},
		{"<marisol@example.com>", "", "marisol@example.com"},
	}

	for _, testCase := range cases {
		t.Run(testCase.in, func(t *testing.T) {
			got, err := parseRecipients([]string{testCase.in})
			if err != nil {
				t.Fatalf("parseRecipients: %v", err)
			}
			if got[0].Label != testCase.label || got[0].Email != testCase.email {
				t.Errorf("got %+v, want {%q %q}", got[0], testCase.label, testCase.email)
			}
		})
	}

	for _, bad := range []string{"", "   ", "Marisol <>"} {
		if _, err := parseRecipients([]string{bad}); err == nil {
			t.Errorf("%q was accepted as a recipient", bad)
		}
	}
}

// A mint with no --field is refused before anything is resolved. The API would answer
// no_fields, but spending a request on a mistake this visible is not worth it — and the
// mint budget is the tightest in the namespace.
func TestMintRefusesWithNoFields(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if code := h.run("shares", "mint", "--to", "Marisol Vega", "--expires", "P7D", "--yes"); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(h.stderr.String(), "dossier fields list") {
		t.Errorf("the refusal does not say how to find a field id:\n%s", h.stderr.String())
	}
}

func TestMintRefusesBothExpiryFlags(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	if code := h.run(mintArgs("--expires", "P7D", "--no-expiry")...); code != exitcode.Usage {
		t.Fatalf("exit = %d, want %d", code, exitcode.Usage)
	}
}

// --no-expiry sends an explicit JSON null, which is the only spelling of the exception the
// API accepts. An empty string or a false would both be a 422 — and one of them is the
// mistake a client emitting "no expiry" would plausibly make.
func TestMintNoExpirySendsAnExplicitNull(t *testing.T) {
	var sent []byte
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			sent = readAll(t, r)
			jsonResponse(http.StatusCreated,
				strings.Replace(mintFixture, `"expires_at":"2026-09-21T09:00:00Z"`, `"expires_at":null`, 1))(w, r)
		},
	})

	if code := h.run(mintArgs("--no-expiry")...); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, h.stderr.String())
	}
	if !strings.Contains(string(sent), `"expires_at":null`) {
		t.Errorf("the body does not carry an explicit null:\n%s", sent)
	}
	if !strings.Contains(h.stdout.String(), "EXPIRES    No expiry") {
		t.Errorf("the block does not name the exception:\n%s", h.stdout.String())
	}
}

// A key supplied by hand that could name a path is refused before it is used as a
// filename. The ledger writes one file per key, so this is a traversal check as much as a
// validation.
func TestMintRefusesAKeyThatIsAPath(t *testing.T) {
	h, _ := newMintHarness(t, map[string]http.HandlerFunc{})

	// An empty value is not in this list: --idempotency-key "" is indistinguishable from
	// not passing the flag, and is treated as absent rather than as a bad key.
	for _, key := range []string{"../../etc/passwd", "a/b", "with space", strings.Repeat("k", 256)} {
		h.reset()
		code := h.run(mintArgs("--expires", "P7D", "--idempotency-key", key)...)
		if code != exitcode.Usage {
			t.Errorf("--idempotency-key %q exited %d, want %d", key, code, exitcode.Usage)
		}
	}
}
