package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
)

// The fixtures below are the shapes the live API returns, arbitrated by the contract
// suite. Every state and every deadline case appears, because the DEADLINE column is the
// one derived figure in the product and each of its four branches is a different rule.
//
// Timestamps are relative to fixedNow (2026-09-16T12:00:00Z) so the goldens are stable:
// the live share expires in 5d 15h, the expiring one in 1h 07m — the two figures the
// plan's §7.3 example shows.
const sharesPageFixture = `{
  "shares": [
    {
      "id": 88,
      "token": "K7M2P9QRX",
      "title": "Lease application",
      "recipient": { "label": "Marisol Vega", "email": "marisol@example.com" },
      "state": "live",
      "expires_at": "2026-09-22T03:00:00Z",
      "revoked_at": null,
      "revoked_reason": null,
      "burn_after_read": true,
      "allow_document_download": false,
      "opens_count": 0,
      "field_count": 1,
      "batch_id": "b6f2b3f0-9e7a-4c8e-9d21-2f6a8b3c4d5e"
    },
    {
      "id": 87,
      "token": "Q2WXM9KRP",
      "title": "Bank KYC",
      "recipient": { "label": "Tomás Herrera", "email": "tomas@example.com" },
      "state": "expiring",
      "expires_at": "2026-09-16T13:07:00Z",
      "revoked_at": null,
      "revoked_reason": null,
      "burn_after_read": false,
      "allow_document_download": true,
      "opens_count": 2,
      "field_count": 3,
      "batch_id": "c7a3d4e1-1f2b-4a5c-8d6e-3f7a9b2c4d5e"
    },
    {
      "id": 84,
      "token": "M3QX7KPR2",
      "title": "Standing employer record",
      "recipient": { "label": "", "email": "" },
      "state": "live",
      "expires_at": null,
      "revoked_at": null,
      "revoked_reason": null,
      "burn_after_read": false,
      "allow_document_download": false,
      "opens_count": 5,
      "field_count": 2,
      "batch_id": null
    },
    {
      "id": 83,
      "token": "W9KMR2PQX",
      "title": "Withdrawn background check",
      "recipient": { "label": "Priya Raman", "email": "priya@example.com" },
      "state": "revoked",
      "expires_at": "2026-10-16T09:00:00Z",
      "revoked_at": "2026-09-14T17:42:00Z",
      "revoked_reason": "holder_revoked",
      "burn_after_read": false,
      "allow_document_download": false,
      "opens_count": 1,
      "field_count": 4,
      "batch_id": "d8b4e5f2-2a3c-4b6d-9e7f-4a8b3c5d6e7f"
    },
    {
      "id": 81,
      "token": "R7KPX2MQ9",
      "title": null,
      "recipient": { "label": "", "email": "consulate@example.com" },
      "state": "expired",
      "expires_at": "2026-09-01T09:00:00Z",
      "revoked_at": null,
      "revoked_reason": null,
      "burn_after_read": false,
      "allow_document_download": false,
      "opens_count": 1,
      "field_count": 2,
      "batch_id": null
    }
  ],
  "next_after": null
}`

// Eight fields spanning the four statuses and a nested documents array, because the DOCS
// column is the one reshaping §7.3 permits and a count of zero must be distinguishable
// from a count of two.
const fieldsPageFixture = `{
  "fields": [
    {
      "id": 9,
      "label": "Legal name (as printed)",
      "category": "identity",
      "country": null,
      "sensitivity": 1,
      "status": "active",
      "has_document": false,
      "documents": []
    },
    {
      "id": 12,
      "label": "U.S. Passport",
      "category": "travel",
      "country": "USA",
      "sensitivity": 3,
      "status": "active",
      "has_document": true,
      "documents": [
        { "id": 3, "filename": "passport-scan.pdf", "content_type": "application/pdf", "byte_size": 248131 },
        { "id": 4, "filename": "passport-back.jpg", "content_type": "image/jpeg", "byte_size": 91020 }
      ]
    },
    {
      "id": 14,
      "label": "Provincial health card",
      "category": "health",
      "country": "CAN",
      "sensitivity": 2,
      "status": "withdrawn",
      "has_document": false,
      "documents": []
    },
    {
      "id": 15,
      "label": "Employer reference",
      "category": "employment",
      "country": null,
      "sensitivity": 1,
      "status": "empty",
      "has_document": false,
      "documents": []
    }
  ],
  "next_after": null
}`

const shareDetailFixture = `{
  "share": {
    "id": 83,
    "token": "W9KMR2PQX",
    "title": "Withdrawn background check",
    "recipient": { "label": "Priya Raman", "email": "priya@example.com" },
    "state": "revoked",
    "expires_at": "2026-10-16T09:00:00Z",
    "revoked_at": "2026-09-14T17:42:00Z",
    "revoked_reason": "holder_revoked",
    "burn_after_read": false,
    "allow_document_download": false,
    "opens_count": 1,
    "field_count": 4,
    "batch_id": "d8b4e5f2-2a3c-4b6d-9e7f-4a8b3c5d6e7f"
  },
  "audit_events": [
    {
      "kind": "revoked",
      "label": "Revoked",
      "meaning": "The holder killed it early. It stopped working instantly.",
      "occurred_at": "2026-09-14T17:42:00Z",
      "detail": "Killed from the shares list"
    },
    {
      "kind": "opened",
      "label": "Opened",
      "meaning": "The dossier was rendered for the recipient.",
      "occurred_at": "2026-09-13T08:15:00Z",
      "detail": null
    },
    {
      "kind": "issued",
      "label": "Issued",
      "meaning": "The dossier was sealed and the link created.",
      "occurred_at": "2026-09-12T11:00:00Z",
      "detail": "4 fields released to Priya Raman"
    }
  ]
}`

// readsHarness wires a harness to a fake serving the schema plus whatever routes a test
// needs, with a token stored so the commands get past requireToken.
func readsHarness(t *testing.T, routes map[string]http.HandlerFunc) *harness {
	t.Helper()

	h := newHarness(t)
	if routes == nil {
		routes = map[string]http.HandlerFunc{}
	}
	if _, ok := routes["/api/v1/schema"]; !ok {
		routes["/api/v1/schema"] = jsonResponse(http.StatusOK, schemaFixture)
	}
	server := fakeAPI(t, routes)

	h.seedProfile("default", store.Profile{
		Host:  server.URL,
		Token: "dsk_" + strings.Repeat("a", 40),
	})
	h.seedDefaultProfile("default")
	return h
}

func TestSharesListGolden(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusOK, sharesPageFixture),
	})

	if code := h.run("shares", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	assertGolden(t, "shares-list.txt", h.stdout.String())
}

// The same page in colour. §7.2 requires the state to be complete as text before colour
// is applied, so this golden is the plain one with SGR sequences added and nothing else
// changed — which is asserted directly rather than eyeballed, and which is also the check
// that the table's padding discounts the invisible bytes.
func TestSharesListInColourIsThePlainOutputPlusColour(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusOK, sharesPageFixture),
	})
	h.app.ForceColor = func() bool { return true }

	if code := h.run("shares", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	coloured := h.stdout.String()
	if !strings.Contains(coloured, "\x1b[") {
		t.Fatal("expected SGR sequences when stdout is a terminal and NO_COLOR is unset")
	}
	assertGolden(t, "shares-list.txt", render.StripSGR(coloured))
}

// Rule 1: colour is state and nothing else. Not the header, not the id, not the title.
func TestSharesListColoursOnlyTheStateWordAndCountdown(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusOK, sharesPageFixture),
	})
	h.app.ForceColor = func() bool { return true }

	if code := h.run("shares", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	for _, coloured := range colouredRuns(h.stdout.String()) {
		if !isStateOrCountdown(coloured) {
			t.Errorf("colour applied to %q, which is neither a state word nor a countdown", coloured)
		}
	}
}

// Expired is deliberately hueless — "absence, not an alarm" on the web, and the same here.
// A terminal that painted it red would turn a deadline that simply arrived into a failure.
func TestSharesListLeavesExpiredHueless(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusOK, sharesPageFixture),
	})
	h.app.ForceColor = func() bool { return true }

	if code := h.run("shares", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	for _, line := range strings.Split(h.stdout.String(), "\n") {
		if strings.Contains(line, "EXPIRED") && strings.Contains(line, "\x1b[") {
			t.Errorf("the expired row carries colour: %q", line)
		}
	}
}

func TestFieldsListGolden(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/fields": jsonResponse(http.StatusOK, fieldsPageFixture),
	})

	if code := h.run("fields", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	assertGolden(t, "fields-list.txt", h.stdout.String())
}

// M1's done-when, and product rule 5 from the outside: the API sends no value and no mask,
// so no rendering of any fixture may contain one. Asserted against the *output*, not
// against the struct, because a struct with no field for a value can still be given one by
// a renderer that reaches into the raw JSON.
func TestFieldsListNeverRendersAValue(t *testing.T) {
	// A server that answers with values anyway — a compromised or future one — must still
	// not get them on screen. The renderer has no column for them, and this is the test
	// that says so rather than trusting it.
	leaky := strings.Replace(
		fieldsPageFixture,
		`"label": "U.S. Passport",`,
		`"label": "U.S. Passport", "value": "X41577255", "value_mask": "·········",`,
		1,
	)

	for name, body := range map[string]string{"ordinary": fieldsPageFixture, "a server sending values": leaky} {
		t.Run(name, func(t *testing.T) {
			h := readsHarness(t, map[string]http.HandlerFunc{
				"/api/v1/fields": jsonResponse(http.StatusOK, body),
			})

			if code := h.run("fields", "list"); code != exitcode.OK {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr)
			}

			out := h.stdout.String() + h.stderr.String()
			for _, forbidden := range []string{"X41577255", "·", "VALUE", "value"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("output contains %q:\n%s", forbidden, out)
				}
			}
		})
	}
}

func TestSharesShowGolden(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares/83": jsonResponse(http.StatusOK, shareDetailFixture),
	})

	if code := h.run("shares", "show", "83"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	assertGolden(t, "shares-show.txt", h.stdout.String())
}

func TestSharesShowWithoutAudit(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares/83": jsonResponse(http.StatusOK, shareDetailFixture),
	})

	if code := h.run("shares", "show", "83", "--no-audit"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	out := h.stdout.String()
	if strings.Contains(out, "AUDIT") {
		t.Errorf("--no-audit still printed the trail:\n%s", out)
	}
	if !strings.Contains(out, "W9KMR2PQX") {
		t.Errorf("the share block should still print:\n%s", out)
	}
}

// Rule 9: every event carries a name *and* the sentence saying what it means, both the
// server's own words. A CLI that printed only the label would have dropped the half of the
// rule that makes the log honest to someone who has never seen it before.
func TestSharesShowPrintsBothTheNameAndTheMeaning(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares/83": jsonResponse(http.StatusOK, shareDetailFixture),
	})

	if code := h.run("shares", "show", "83"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	out := h.stdout.String()
	for _, want := range []string{
		"Revoked", "The holder killed it early. It stopped working instantly.",
		"Opened", "The dossier was rendered for the recipient.",
		"Issued", "The dossier was sealed and the link created.",
		"Killed from the shares list", "4 fields released to Priya Raman",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from:\n%s", want, out)
		}
	}
}

// §7.4: the body byte-for-byte, nothing else on stdout, and it must survive a parser.
func TestJSONRoundTripsEveryResponse(t *testing.T) {
	cases := []struct {
		name  string
		route string
		body  string
		args  []string
	}{
		{"shares list", "/api/v1/shares", sharesPageFixture, []string{"shares", "list"}},
		{"fields list", "/api/v1/fields", fieldsPageFixture, []string{"fields", "list"}},
		{"shares show", "/api/v1/shares/83", shareDetailFixture, []string{"shares", "show", "83"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := readsHarness(t, map[string]http.HandlerFunc{
				tc.route: jsonResponse(http.StatusOK, tc.body),
			})

			if code := h.run(append(tc.args, "--json")...); code != exitcode.OK {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr)
			}

			var got, want any
			if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, h.stdout)
			}
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatalf("fixture is not JSON: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("--json altered the body\n--- got ---\n%s\n--- want ---\n%s", h.stdout, tc.body)
			}
			if h.stderr.Len() != 0 {
				t.Errorf("--json wrote prose to stderr, which a piped run should not need: %s", h.stderr)
			}
		})
	}
}

// --all follows the cursor until the server stops offering one, and specifically does not
// stop on a short page. The API applies the state filter in Ruby after fetching, so a
// filtered page can be shorter than the limit — or empty — while matches sit further back.
// Stopping on a short page is the obvious implementation and it silently truncates.
func TestSharesListAllFollowsTheCursorPastShortPages(t *testing.T) {
	pages := []string{
		`{"shares": [{"id": 88, "token": "K7M2P9QRX", "title": "One", "recipient": {"label": "A", "email": "a@example.com"}, "state": "live", "expires_at": "2026-09-22T03:00:00Z", "revoked_at": null, "opens_count": 0, "field_count": 1, "batch_id": null}], "next_after": 88}`,
		// Deliberately empty, with a cursor: every row on this page was filtered out.
		`{"shares": [], "next_after": 60}`,
		`{"shares": [{"id": 55, "token": "R7KPX2MQ9", "title": "Two", "recipient": {"label": "B", "email": "b@example.com"}, "state": "live", "expires_at": "2026-09-23T03:00:00Z", "revoked_at": null, "opens_count": 0, "field_count": 1, "batch_id": null}], "next_after": null}`,
	}

	var requested []string
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			requested = append(requested, r.URL.Query().Get("after"))
			body := pages[min(len(requested)-1, len(pages)-1)]
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		},
	})

	if code := h.run("shares", "list", "--state", "live", "--all"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}

	if len(requested) != 3 {
		t.Fatalf("expected three requests following the cursor, got %d: %q", len(requested), requested)
	}
	if requested[0] != "" || requested[1] != "88" || requested[2] != "60" {
		t.Errorf("cursors not followed in order: %q", requested)
	}

	out := h.stdout.String()
	// The row beyond the empty page is the one a short-page stop would have lost.
	if !strings.Contains(out, "R7KPX2MQ9") {
		t.Errorf("the row past the empty page is missing:\n%s", out)
	}
	if !strings.Contains(out, "K7M2P9QRX") {
		t.Errorf("the first page's row is missing:\n%s", out)
	}
	if strings.Count(out, "ID ") != 1 {
		t.Errorf("--all should print one header, not one per page:\n%s", out)
	}
}

// Without --all, one page and no cursor following, even when the server offers one.
func TestSharesListWithoutAllFetchesOnePage(t *testing.T) {
	var requests int
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"shares": [], "next_after": 88}`))
		},
	})

	if code := h.run("shares", "list"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	if requests != 1 {
		t.Errorf("made %d requests without --all, want 1", requests)
	}
}

func TestListFiltersAreValidatedAgainstTheSchema(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		bad      string
		expected string
	}{
		{"an unknown share state", []string{"shares", "list", "--state", "lve"}, "lve", "live, expiring, expired, revoked"},
		{"an unknown field status", []string{"fields", "list", "--status", "gone"}, "gone", "empty, active, pending, withdrawn"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			h := readsHarness(t, map[string]http.HandlerFunc{
				"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) { reached = true },
				"/api/v1/fields": func(w http.ResponseWriter, r *http.Request) { reached = true },
			})

			code := h.run(tc.args...)

			if code != exitcode.Usage {
				t.Errorf("exit %d, want %d — a refusal before sending anything", code, exitcode.Usage)
			}
			if reached {
				t.Error("the CLI sent the request anyway")
			}
			// The vocabulary comes from the schema, so the message can name it. A
			// hardcoded list in the binary is the thing this is checking has not happened.
			if !strings.Contains(h.stderr.String(), tc.expected) {
				t.Errorf("the refusal should name what the API publishes, got:\n%s", h.stderr)
			}
		})
	}
}

// A value the schema does know is passed through as the query parameter.
func TestListFiltersAreSentAsQueryParameters(t *testing.T) {
	var query string
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"shares": [], "next_after": null}`))
		},
	})

	if code := h.run("shares", "list", "--state", "revoked", "--limit", "7"); code != exitcode.OK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr)
	}
	for _, want := range []string{"state=revoked", "limit=7"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}
}

// An empty list is not an error, and the note goes to stderr so `--json | jq` sees an
// empty result rather than prose.
func TestAnEmptyListingExitsZeroAndSaysSoOnStderr(t *testing.T) {
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares": jsonResponse(http.StatusOK, `{"shares": [], "next_after": null}`),
	})

	if code := h.run("shares", "list", "--state", "revoked"); code != exitcode.OK {
		t.Fatalf("exit %d, want 0 — no rows is an answer, not a failure", code)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", h.stdout)
	}
	if !strings.Contains(h.stderr.String(), "--state revoked") {
		t.Errorf("the note should name the filter that matched nothing, got %q", h.stderr)
	}
}

func TestSharesShowRefusesANonNumericID(t *testing.T) {
	var reached bool
	h := readsHarness(t, map[string]http.HandlerFunc{
		"/api/v1/shares/": func(w http.ResponseWriter, r *http.Request) { reached = true },
	})

	if code := h.run("shares", "show", "K7M2P9QRX"); code != exitcode.Usage {
		t.Errorf("exit %d, want %d", code, exitcode.Usage)
	}
	if reached {
		t.Error("the CLI sent a request for a token where an id belongs")
	}
}

func TestReadsRefuseWithoutAToken(t *testing.T) {
	for _, args := range [][]string{
		{"shares", "list"}, {"fields", "list"}, {"shares", "show", "1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t)
			server := fakeAPI(t, map[string]http.HandlerFunc{
				"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
			})
			h.app.HostFlag = server.URL

			if code := h.run(args...); code != exitcode.Usage {
				t.Errorf("exit %d, want %d", code, exitcode.Usage)
			}
		})
	}
}

// The error envelope's own copy, verbatim, and the exit code its API code maps to.
func TestReadsMapAPIFailuresToExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   int
	}{
		{"a missing scope", http.StatusForbidden, "insufficient_scope", exitcode.ScopeMissing},
		{"a dead token", http.StatusUnauthorized, "unauthenticated", exitcode.Unauthenticated},
		{"someone else's share", http.StatusNotFound, "not_found", exitcode.NotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := readsHarness(t, map[string]http.HandlerFunc{
				"/api/v1/shares": errorEnvelope(tc.status, tc.code, "The API's own sentence.", "The API's own hint."),
			})

			if code := h.run("shares", "list", "--no-wait"); code != tc.want {
				t.Errorf("exit %d, want %d (stderr: %s)", code, tc.want, h.stderr)
			}
			if !strings.Contains(h.stderr.String(), "The API's own sentence.") {
				t.Errorf("the envelope's message should be verbatim, got:\n%s", h.stderr)
			}
		})
	}
}

// colouredRuns returns the text inside each SGR pair.
func colouredRuns(s string) []string {
	var runs []string
	for {
		start := strings.Index(s, "\x1b[")
		if start < 0 {
			return runs
		}
		open := strings.IndexByte(s[start:], 'm')
		if open < 0 {
			return runs
		}
		rest := s[start+open+1:]
		end := strings.Index(rest, "\x1b[")
		if end < 0 {
			return append(runs, rest)
		}
		runs = append(runs, rest[:end])
		s = rest[end:]
		// Skip the reset so its empty payload is not counted as a coloured run.
		if reset := strings.IndexByte(s, 'm'); reset >= 0 {
			s = s[reset+1:]
		}
	}
}

func isStateOrCountdown(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	for _, state := range []string{"LIVE", "EXPIRING", "REVOKED", "EXPIRED"} {
		if trimmed == state {
			return true
		}
	}
	// A countdown: "5d 15h left", "1h 07m left", or rule 2's labelled exception.
	return strings.HasSuffix(trimmed, "left") || trimmed == render.NoExpiry
}
