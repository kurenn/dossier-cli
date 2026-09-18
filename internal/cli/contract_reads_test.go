//go:build contract

// The M1 half of the contract suite: the read commands, arbitrated against a live server.
//
// The same reasoning as contract_test.go. The fixtures in reads_test.go were written by
// reading docs/api-shares.md, which makes them a restatement of what a reader believed —
// and the first version of them was wrong in a way no unit test could have found, because
// the fake and the renderer agreed with each other. The tests here compare against what
// the server actually sends.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// contractTimeout is generous on purpose: in CI this talks to a Rails app that may be
// serving its first request of the process.
const contractTimeout = 30 * time.Second

// Owner reads are limited to 60/min *per token* (docs/api.md §5), and the limiter is live
// in the test environment because the cache store is a memory store. This file roughly
// doubled the suite's request count, so its tests deliberately read through the
// single-scope tokens rather than putting every request on `all`: three tokens is three
// budgets, and the alternative is a suite that goes red when the machine is fast enough to
// finish inside a minute.
//
// The same reasoning caps the cursor walk below to one state instead of four.
const (
	sharesReader = "shares_read"
	fieldsReader = "fields_read"
)

// TestContractResponseFieldsAreAllDecoded is the test that would have caught the gap that
// started M1.
//
// The schema publishes `response_fields` per endpoint — the keys the API says it returns.
// This asserts that the CLI's structs have a home for every one of them. `revoked_at` was
// missing from the API entirely, and the symptom was a renderer that could not show a
// revoked share's closing fact; once it was added, nothing but a test like this would
// notice if the CLI quietly ignored it.
//
// It is deliberately one-directional: the CLI may model keys the schema does not advertise
// (it lags, and an undocumented key is the server's business), but it may not be blind to
// a key the server publishes. That is the direction drift actually travels.
func TestContractResponseFieldsAreAllDecoded(t *testing.T) {
	schema := liveSchema(t)

	cases := []struct {
		method string
		path   string
		into   any
		// prefix is stripped from each response_field before matching, for the endpoints
		// whose rows are nested under a key ("shares[].id" -> "id").
		prefix string
	}{
		{"GET", "/api/v1/fields", api.Field{}, "fields[]."},
		{"GET", "/api/v1/shares", api.Share{}, "shares[]."},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			endpoint := findEndpoint(t, schema, tc.method, tc.path)
			known := jsonTags(tc.into)

			for _, field := range endpoint.ResponseFields {
				if !strings.HasPrefix(field, tc.prefix) {
					// A top-level key such as `next_after`, modelled on the page struct
					// rather than the row struct. Covered by the decode tests.
					continue
				}
				key := strings.TrimPrefix(field, tc.prefix)
				if !slices.Contains(known, key) {
					t.Errorf("the schema publishes %s but %T has no field for %q.\n"+
						"The API grew a key the CLI ignores; add it to the struct and to the renderer or the fixture.",
						field, tc.into, key)
				}
			}
		})
	}
}

// TestContractShareDetailFieldsAreAllDecoded does the same for the show endpoint, whose
// response_fields name `share` as one opaque key rather than enumerating it. So the check
// has to be against the live body's own keys.
func TestContractShareDetailFieldsAreAllDecoded(t *testing.T) {
	client := contractClient(t, contractFixture(t).Tokens[sharesReader].Raw)
	id := anyShareID(t, client)

	var raw struct {
		Share       map[string]json.RawMessage   `json:"share"`
		AuditEvents []map[string]json.RawMessage `json:"audit_events"`
	}
	if err := getJSON(t, client, "/api/v1/shares/"+fmt.Sprint(id), nil, &raw); err != nil {
		t.Fatalf("GET /shares/%d: %v", id, err)
	}

	shareTags := jsonTags(api.Share{})
	for key := range raw.Share {
		if !slices.Contains(shareTags, key) {
			t.Errorf("the live share object has %q and api.Share does not model it", key)
		}
	}

	if len(raw.AuditEvents) == 0 {
		t.Fatal("the fixture's shares should all have an issued event at least")
	}
	eventTags := jsonTags(api.AuditEvent{})
	for key := range raw.AuditEvents[0] {
		if !slices.Contains(eventTags, key) {
			t.Errorf("the live audit event has %q and api.AuditEvent does not model it", key)
		}
	}
}

// TestContractEveryRevokedShareCarriesARevokedAt is the invariant the DEADLINE column
// rests on, asserted against the server rather than against the fixture's intent.
//
// And the second assertion is the reason the column exists: the fixture's revoked share is
// minted with a month of expiry left and then killed, so `expires_at` is in the future
// while the link refuses every request. A renderer trusting `expires_at` would say "29d
// left" about a dead dossier.
func TestContractEveryRevokedShareCarriesARevokedAt(t *testing.T) {
	client := contractClient(t, contractFixture(t).Tokens[sharesReader].Raw)

	page, err := client.ListShares(context.Background(), api.ShareQuery{State: "revoked", Limit: 100}, contractTimeout)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(page.Shares) == 0 {
		t.Fatal("the fixture should seed a revoked share")
	}

	var sawFutureExpiry bool
	for _, share := range page.Shares {
		if share.RevokedAt == nil {
			t.Errorf("share %d is revoked and carries no revoked_at, so its closing fact is unrenderable", share.ID)
			continue
		}
		if share.ExpiresAt != nil && share.ExpiresAt.After(*share.RevokedAt) {
			sawFutureExpiry = true
		}
	}

	if !sawFutureExpiry {
		t.Error("no revoked share outlives its own revocation, so the fixture no longer covers " +
			"the case the DEADLINE column exists for — a revoked share whose expiry is still ahead")
	}
}

// TestContractSharesListRendersEveryState drives the real command against the real server
// and checks the rendering of each state the schema publishes.
//
// The fixture seeds one share per state, so every branch of render.Deadline is exercised
// against real data: a countdown, the labelled no-expiry exception, a revocation date and
// an expiry timestamp.
func TestContractSharesListRendersEveryState(t *testing.T) {
	data := contractFixture(t)

	harness := newHarness(t)
	t.Setenv("DOSSIER_TOKEN", data.Tokens[sharesReader].Raw)

	if code := harness.run("shares", "list", "--all", "--no-wait", "--host", contractHost(t)); code != exitcode.OK {
		t.Fatalf("shares list exited %d\nstdout:\n%s\nstderr:\n%s", code, harness.stdout, harness.stderr)
	}

	output := harness.stdout.String()
	for _, state := range liveSchema(t).ShareStates {
		if !strings.Contains(output, strings.ToUpper(state)) {
			t.Errorf("no %s row in the listing, so that state's deadline rendering is untested:\n%s",
				strings.ToUpper(state), output)
		}
	}

	// Rule 2's labelled exception, and the two closing facts. Each is a different branch,
	// and each would silently become an em dash if the API stopped sending a key.
	for _, want := range []string{"No expiry", "Revoked ", " left"} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q somewhere in the listing:\n%s", want, output)
		}
	}

	// The trap, stated as an assertion: no row may show a countdown next to REVOKED.
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "REVOKED") && strings.Contains(line, "left") {
			t.Errorf("a revoked share is showing a countdown: %q", line)
		}
	}
}

// TestContractFieldsListNeverRendersAValue is product rule 5 checked end to end, against
// the server that holds the real encrypted values.
//
// The unit test proves the renderer has no column for a value even when handed one. This
// proves the thing that makes that sufficient: the live endpoint does not send one. Both
// halves are needed — a renderer that cannot leak is no help if the API starts sending
// values to a different client, and an API that sends none is no help if the renderer
// starts asking for them.
func TestContractFieldsListNeverRendersAValue(t *testing.T) {
	data := contractFixture(t)

	// Every status, so a `pending` or `withdrawn` row cannot be the one that leaks.
	for _, status := range append([]string{""}, liveSchema(t).FieldStatuses...) {
		harness := newHarness(t)
		t.Setenv("DOSSIER_TOKEN", data.Tokens[fieldsReader].Raw)

		args := []string{"fields", "list", "--all", "--no-wait", "--host", contractHost(t)}
		if status != "" {
			args = append(args, "--status", status)
		}
		if code := harness.run(args...); code != exitcode.OK {
			t.Fatalf("status %q: exited %d\nstderr:\n%s", status, code, harness.stderr)
		}

		output := harness.stdout.String()

		// The column set, asserted exactly. The seed task deliberately never prints field
		// values — it would be writing plaintext identity data to stdout — so there is no
		// known string to search for. What can be checked is stronger anyway: that the
		// table has these columns and no others, so a value column cannot appear without
		// failing here.
		header := strings.Fields(strings.SplitN(output, "\n", 2)[0])
		want := []string{"ID", "LABEL", "CATEGORY", "COUNTRY", "SENSITIVITY", "STATUS", "HAS_DOCUMENT", "DOCS"}
		if !slices.Equal(header, want) {
			t.Errorf("status %q: columns are %v, want exactly %v", status, header, want)
		}
		if strings.Contains(output, "·") {
			t.Errorf("status %q: the listing contains a redaction bar, which this command has no business rendering:\n%s",
				status, output)
		}
	}
}

// TestContractRawFieldBodiesCarryNoValue checks the same boundary one layer lower: not
// what the renderer printed, but what arrived over the socket.
//
// Rule 5 is explicit that a value must never reach the client — "not in the HTML, not in
// JSON, not in a data attribute". So the assertion is on the raw page body, which is also
// what --json emits verbatim.
func TestContractRawFieldBodiesCarryNoValue(t *testing.T) {
	client := contractClient(t, contractFixture(t).Tokens[fieldsReader].Raw)

	for _, status := range append([]string{""}, liveSchema(t).FieldStatuses...) {
		page, err := client.ListFields(context.Background(), api.FieldQuery{Status: status, Limit: 100}, contractTimeout)
		if err != nil {
			t.Fatalf("status %q: %v", status, err)
		}

		body := string(page.Raw)
		for _, key := range []string{`"value"`, `"value_mask"`, `"mask_length"`, `"ciphertext"`} {
			if strings.Contains(body, key) {
				t.Errorf("status %q: the field listing body carries %s:\n%s", status, key, body)
			}
		}

		// Every key in every row, checked against what api.Field models, so a key the CLI
		// does not decode cannot slip past unnoticed — which is how a value would arrive.
		var rows struct {
			Fields []map[string]json.RawMessage `json:"fields"`
		}
		if err := json.Unmarshal(page.Raw, &rows); err != nil {
			t.Fatalf("status %q: %v", status, err)
		}
		known := jsonTags(api.Field{})
		for _, row := range rows.Fields {
			for key := range row {
				if !slices.Contains(known, key) {
					t.Errorf("status %q: the API returned field key %q, which api.Field does not model", status, key)
				}
			}
		}
	}
}

// TestContractAllFollowsTheCursorOnAFilteredListing is the pagination claim in §4.2,
// checked against the behaviour that makes it necessary.
//
// The API derives state at read time and applies `state` in Ruby *after* fetching a page,
// so a filtered page can come back shorter than the limit — or empty — while matching rows
// sit further back. A client that stopped on a short page would silently truncate. With
// `--limit 1` over six seeded shares, every page is short by construction.
func TestContractAllFollowsTheCursorOnAFilteredListing(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, data.Tokens[sharesReader].Raw)

	for _, state := range cursorWalkStates(liveSchema(t).ShareStates) {
		var walked int
		var after *int
		for {
			page, err := client.ListShares(context.Background(),
				api.ShareQuery{State: state, Limit: 1, After: after}, contractTimeout)
			if err != nil {
				t.Fatalf("state %q: %v", state, err)
			}
			walked += len(page.Shares)
			if page.NextAfter == nil {
				break
			}
			after = page.NextAfter
		}

		// One page of 100 is the same answer arrived at differently.
		single, err := client.ListShares(context.Background(),
			api.ShareQuery{State: state, Limit: 100}, contractTimeout)
		if err != nil {
			t.Fatalf("state %q: %v", state, err)
		}

		if walked != len(single.Shares) {
			t.Errorf("state %q: walking one row at a time found %d shares, one page found %d — "+
				"the cursor walk is losing rows on short pages", state, walked, len(single.Shares))
		}
		if walked == 0 {
			t.Errorf("state %q: the fixture seeds no share in this state", state)
		}
	}
}

// TestContractSharesShowMatchesTheListing checks that show and list agree about a share,
// since they are rendered by different code from the same builder.
func TestContractSharesShowMatchesTheListing(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, data.Tokens[sharesReader].Raw)

	page, err := client.ListShares(context.Background(), api.ShareQuery{Limit: 100}, contractTimeout)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}

	for _, listed := range page.Shares {
		detail, err := client.ShowShare(context.Background(), listed.ID, contractTimeout)
		if err != nil {
			t.Fatalf("ShowShare(%d): %v", listed.ID, err)
		}
		if !reflect.DeepEqual(listed, detail.Share) {
			t.Errorf("share %d differs between list and show:\nlist: %+v\nshow: %+v",
				listed.ID, listed, detail.Share)
		}
		if len(detail.AuditEvents) == 0 {
			t.Errorf("share %d has an empty audit trail, which a minted share cannot have", listed.ID)
		}
	}
}

// TestContractAuditTrailCarriesRule9sPair asserts the log is honest in the form rule 9
// requires: every event has a name *and* a sentence, and the CLI renders both.
//
// The revoked share is the specific one checked, because the fixture used to revoke it by
// writing the column directly and bypassing `Shares::Revoke` — which left the one revoked
// share in the system with no `revoked` audit row at all, and would have taught this
// command that the API does not record revocations.
func TestContractAuditTrailCarriesRule9sPair(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, data.Tokens[sharesReader].Raw)

	// By token, not "the first revoked share in the list", which is what this used to do.
	// M4's burn test spends the fixture's burn share, and a spent one is also revoked —
	// so the first row became a share whose trail carries `burned` rather than `revoked`,
	// and this test failed claiming the product rule had regressed. The share it means is
	// the one the fixture revoked by hand, and it can say so.
	token, _ := shareByKey(t, data, "revoked")
	id := shareIDByToken(t, client, token)

	detail, err := client.ShowShare(context.Background(), id, contractTimeout)
	if err != nil {
		t.Fatalf("ShowShare: %v", err)
	}

	var sawRevokedEvent bool
	for _, event := range detail.AuditEvents {
		if event.Kind == "revoked" {
			sawRevokedEvent = true
		}
		if event.Label == "" || event.Meaning == "" {
			t.Errorf("event %q carries label %q and meaning %q — rule 9 needs both",
				event.Kind, event.Label, event.Meaning)
		}
		if !strings.HasSuffix(strings.TrimSpace(event.Meaning), ".") {
			t.Errorf("event %q's meaning is not a sentence: %q", event.Kind, event.Meaning)
		}
	}
	if !sawRevokedEvent {
		t.Error("a revoked share's trail has no `revoked` event. Either the fixture revoked it " +
			"without going through Shares::Revoke, or the product rule that every revoke path " +
			"logs `revoked` has regressed")
	}

	// And the rendering: both halves on screen, the server's words verbatim.
	harness := newHarness(t)
	t.Setenv("DOSSIER_TOKEN", data.Tokens[sharesReader].Raw)
	if code := harness.run("shares", "show", fmt.Sprint(id), "--no-wait", "--host", contractHost(t)); code != exitcode.OK {
		t.Fatalf("shares show exited %d\nstderr:\n%s", code, harness.stderr)
	}

	output := harness.stdout.String()
	for _, event := range detail.AuditEvents {
		if !strings.Contains(output, event.Label) {
			t.Errorf("the rendering omits the label %q:\n%s", event.Label, output)
		}
		if !strings.Contains(output, event.Meaning) {
			t.Errorf("the rendering omits the meaning %q:\n%s", event.Meaning, output)
		}
	}
	if !strings.Contains(output, "Revoked "+detail.Share.RevokedAt.UTC().Format("2006-01-02")) {
		t.Errorf("the DEADLINE should show the revocation date:\n%s", output)
	}
}

// TestContractJSONIsTheServersOwnBytes checks §7.4 against a live body: --json emits what
// arrived, not a re-serialisation of the decoded struct.
func TestContractJSONIsTheServersOwnBytes(t *testing.T) {
	data := contractFixture(t)

	for _, args := range [][]string{
		{"shares", "list"},
		{"fields", "list"},
	} {
		harness := newHarness(t)
		t.Setenv("DOSSIER_TOKEN", data.Tokens["all"].Raw)

		full := append(args, "--json", "--no-wait", "--host", contractHost(t))
		if code := harness.run(full...); code != exitcode.OK {
			t.Fatalf("%v exited %d\nstderr:\n%s", args, code, harness.stderr)
		}

		// Parses, and stderr carries nothing a pipe would have to filter.
		var body map[string]any
		if err := json.Unmarshal(harness.stdout.Bytes(), &body); err != nil {
			t.Errorf("%v: stdout is not one JSON document: %v\n%s", args, err, harness.stdout)
		}
		if harness.stderr.Len() != 0 {
			t.Errorf("%v: --json wrote to stderr: %s", args, harness.stderr)
		}
	}
}

// TestContractReadsRefuseWithoutTheScope checks the 403 path for the read commands, and
// that the CLI reports the API's own sentence rather than its own guess.
func TestContractReadsRefuseWithoutTheScope(t *testing.T) {
	data := contractFixture(t)

	cases := []struct {
		handle string
		args   []string
	}{
		// A token holding only shares:read cannot list fields, and vice versa.
		{"shares_read", []string{"fields", "list"}},
		{"fields_read", []string{"shares", "list"}},
	}

	for _, tc := range cases {
		token, seeded := data.Tokens[tc.handle]
		if !seeded {
			t.Fatalf("the fixture has no %q token", tc.handle)
		}

		harness := newHarness(t)
		t.Setenv("DOSSIER_TOKEN", token.Raw)

		full := append(tc.args, "--no-wait", "--host", contractHost(t))
		code := harness.run(full...)

		if code == exitcode.TryLater {
			t.Skipf("%v: rate limited; the per-IP limiter is shared with the rest of this suite", tc.args)
		}
		if code != exitcode.ScopeMissing {
			t.Errorf("%v with %q exited %d, want %d\nstderr:\n%s",
				tc.args, tc.handle, code, exitcode.ScopeMissing, harness.stderr)
		}
		if harness.stdout.Len() != 0 {
			t.Errorf("%v: a refused command wrote to stdout: %s", tc.args, harness.stdout)
		}
	}
}

// findEndpoint locates one endpoint in the schema, failing if the server stopped
// publishing it.
func findEndpoint(t *testing.T, schema *api.Schema, method, path string) api.Endpoint {
	t.Helper()

	for _, endpoint := range schema.Endpoints {
		if endpoint.Method == method && endpoint.Path == path {
			return endpoint
		}
	}
	t.Fatalf("the live schema publishes no %s %s", method, path)
	return api.Endpoint{}
}

// jsonTags lists the json tag names of a struct's fields, ignoring those marked "-".
func jsonTags(v any) []string {
	typ := reflect.TypeOf(v)
	tags := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			tags = append(tags, name)
		}
	}
	return tags
}

// cursorWalkStates picks the one state worth walking a row at a time.
//
// The walk costs a request per share in the vault regardless of the filter, because
// `next_after` advances by id and not by match — which is the very property under test.
// Four states therefore cost four full traversals, and the suite has a 60/min budget per
// token. `live` is chosen because the fixture seeds three live shares among six, so the
// pages after the third are exactly the short-and-cursored ones a naive client truncates
// on; a state with a single match would walk the same distance and prove less.
func cursorWalkStates(published []string) []string {
	if slices.Contains(published, "live") {
		return []string{"live"}
	}
	if len(published) > 0 {
		return published[:1]
	}
	return nil
}

func anyShareID(t *testing.T, client *api.Client) int {
	t.Helper()

	page, err := client.ListShares(context.Background(), api.ShareQuery{Limit: 1}, contractTimeout)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(page.Shares) == 0 {
		t.Fatal("the fixture seeds no shares")
	}
	return page.Shares[0].ID
}
