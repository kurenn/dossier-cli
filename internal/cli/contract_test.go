//go:build contract

// The contract suite. Excluded from a normal `go test ./...` by the build tag above,
// because unlike every other test in this repository it needs a real Dossier on a socket.
//
// Why it exists: every other test in this package drives the CLI against `fakeAPI` and
// `schemaFixture`, which are hand-written from docs/api.md. A fake derived from
// documentation is a restatement of what someone believed the server does, and it agrees
// with the client by construction — including when both are wrong. That is a suite which
// certifies the CLI against its author's own assumptions.
//
// So this file's job is not to re-test the CLI. It is to arbitrate the fakes: to assert
// that the vocabularies `schemaFixture` declares are the ones a live server publishes,
// and that the envelopes the client switches on are envelopes the API really sends. When
// it fails, the bug is usually in the fake, not in the code under test.
//
// Run it with:
//
//	go test -tags contract -run TestContract ./internal/cli
//
// against a server booted in the test environment with `rails cli:seed_contract` already
// run. See .github/workflows/ci.yml in the dossier repository, which is what actually
// does this on every push.
//
// Nothing here runs in parallel and the order of the tests is load-bearing: the last one
// deliberately trips the failed-authentication rate limiter, which is per-IP.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// contractData is the JSON that `rails cli:seed_contract` prints. Only the parts this
// suite asserts on are modelled; the task is free to add more.
//
// `key` on a field or share is a fixture-local handle, not something the API returns.
// It is how a test names the row it means without depending on a database id.
type contractData struct {
	Holder struct {
		Email      string `json:"email"`
		Passphrase string `json:"passphrase"`
	} `json:"holder"`

	Tokens map[string]struct {
		Raw    string   `json:"raw"`
		Scopes []string `json:"scopes"`
		State  string   `json:"state"`
	} `json:"tokens"`

	Fields []struct {
		ID     int    `json:"id"`
		Key    string `json:"key"`
		Status string `json:"status"`
	} `json:"fields"`

	Shares []struct {
		Key   string `json:"key"`
		Token string `json:"token"`
		PIN   string `json:"pin"`
		State string `json:"state"`
	} `json:"shares"`
}

const (
	hostEnv    = "DOSSIER_CONTRACT_HOST"
	fixtureEnv = "DOSSIER_CONTRACT_FIXTURE"
)

func contractHost(t *testing.T) string {
	t.Helper()

	host := os.Getenv(hostEnv)
	if host == "" {
		t.Fatalf("%s is not set.\n\nThe contract suite needs a live server; it has no fake to fall back on.\nBoot one with `RAILS_ENV=test bin/rails server` and set %s to its base URL.", hostEnv, hostEnv)
	}
	return host
}

func contractFixture(t *testing.T) contractData {
	t.Helper()

	path := os.Getenv(fixtureEnv)
	if path == "" {
		path = filepath.Join("..", "..", "testdata", "contract.json")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the contract fixture at %s: %v\n\nGenerate it with `RAILS_ENV=test bin/rails cli:seed_contract > %s`\nin the dossier repository, against the same database the server is using.", path, err, path)
	}

	var data contractData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if len(data.Tokens) == 0 {
		t.Fatalf("%s carries no tokens; it cannot be the seed task's output", path)
	}
	return data
}

// contractClient is an API client pointed at the live server.
//
// WithNoWait is not an optimisation. The client's default behaviour on a 429 is to honour
// Retry-After and sleep, and this API's Retry-After is a full minute — so a suite that
// let it retry would hang for minutes the moment it touched a limit, including in the one
// test that trips a limiter on purpose.
func contractClient(t *testing.T, token string) *api.Client {
	t.Helper()

	client, err := api.New(contractHost(t), api.WithToken(token), api.WithNoWait(true))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

// activeTokens are the seeded tokens that should authenticate, keyed by fixture handle.
// The dead ones are excluded because every request with one spends a unit of the per-IP
// failed-authentication budget, which is the scarcest resource in this suite.
func activeTokens(data contractData) map[string][]string {
	active := map[string][]string{}
	for handle, token := range data.Tokens {
		if token.State == "active" {
			active[handle] = token.Scopes
		}
	}
	return active
}

// liveSchema fetches the discovery document. Anonymous, so it costs nothing from any
// token's budget.
func liveSchema(t *testing.T) *api.Schema {
	t.Helper()

	client, err := api.New(contractHost(t), api.WithNoWait(true))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	schema, err := client.FetchSchema(context.Background())
	if err != nil {
		t.Fatalf("GET /api/v1/schema: %v\n\nIs a server running at %s?", err, contractHost(t))
	}
	return schema
}

// fakeSchema decodes schemaFixture, the document every other test in this package
// pretends the server sent.
func fakeSchema(t *testing.T) *api.Schema {
	t.Helper()

	var schema api.Schema
	if err := json.Unmarshal([]byte(schemaFixture), &schema); err != nil {
		t.Fatalf("schemaFixture is not valid JSON: %v", err)
	}
	return &schema
}

// ---------------------------------------------------------------------------
// 1. The fake's vocabularies against the real ones
// ---------------------------------------------------------------------------

// TestContractSchemaMatchesTheFake is the assertion the whole job exists for.
//
// The closed vocabularies are compared order-sensitively on purpose. The CLI renders
// these lists in the order the server sends them, and the golden files record that order,
// so a reordering is a real change to the product's output even though the set is equal.
func TestContractSchemaMatchesTheFake(t *testing.T) {
	live := liveSchema(t)
	fake := fakeSchema(t)

	vocabularies := []struct {
		name string
		fake []string
		live []string
	}{
		{"scopes", fake.Scopes, live.Scopes},
		{"field_categories", fake.FieldCategories, live.FieldCategories},
		{"field_statuses", fake.FieldStatuses, live.FieldStatuses},
		{"share_states", fake.ShareStates, live.ShareStates},
	}

	for _, vocabulary := range vocabularies {
		if len(vocabulary.live) == 0 {
			t.Errorf("the server published no %s at all", vocabulary.name)
			continue
		}
		if !slices.Equal(vocabulary.fake, vocabulary.live) {
			t.Errorf("schemaFixture's %s have drifted from the server.\n  fake: %v\n  live: %v\n\nFix the fake in helpers_test.go, then re-run the golden tests with -update.",
				vocabulary.name, vocabulary.fake, vocabulary.live)
		}
	}

	// Expiry is product copy, and CLAUDE.md is explicit that a paraphrase is a defect —
	// so it is asserted verbatim rather than for shape. The no-expiry label and note
	// carry rule 2 ("expiry is required, not a setting"); if the server ever stops
	// calling it the exception, the CLI must stop calling it that too.
	if live.Expiry.Required != fake.Expiry.Required {
		t.Errorf("expiry.required: fake %v, live %v", fake.Expiry.Required, live.Expiry.Required)
	}
	if !slices.Equal(fake.Expiry.Presets, live.Expiry.Presets) {
		t.Errorf("expiry.presets: fake %v, live %v", fake.Expiry.Presets, live.Expiry.Presets)
	}
	if fake.Expiry.Note != live.Expiry.Note {
		t.Errorf("expiry.note has drifted.\n  fake: %q\n  live: %q", fake.Expiry.Note, live.Expiry.Note)
	}
	if fake.Expiry.NoExpiry.Label != live.Expiry.NoExpiry.Label {
		t.Errorf("expiry.no_expiry.label: fake %q, live %q", fake.Expiry.NoExpiry.Label, live.Expiry.NoExpiry.Label)
	}
	if fake.Expiry.NoExpiry.Note != live.Expiry.NoExpiry.Note {
		t.Errorf("expiry.no_expiry.note has drifted.\n  fake: %q\n  live: %q", fake.Expiry.NoExpiry.Note, live.Expiry.NoExpiry.Note)
	}

	// Endpoints and error codes are subsets by design — the fake carries the few rows its
	// golden output needs. A subset is still worth checking: every row it does carry must
	// describe a real endpoint with the real scope, because the scope is what the CLI
	// tells the holder they need.
	for _, endpoint := range fake.Endpoints {
		index := slices.IndexFunc(live.Endpoints, func(candidate api.Endpoint) bool {
			return candidate.Method == endpoint.Method && candidate.Path == endpoint.Path
		})
		if index < 0 {
			t.Errorf("schemaFixture claims %s %s, which the server does not publish", endpoint.Method, endpoint.Path)
			continue
		}
		if live.Endpoints[index].Scope != endpoint.Scope {
			t.Errorf("%s %s: fake says scope %q, server says %q",
				endpoint.Method, endpoint.Path, endpoint.Scope, live.Endpoints[index].Scope)
		}
	}

	liveStatus := map[string]int{}
	for _, row := range live.ErrorCodes {
		liveStatus[row.Code] = row.Status
	}
	for _, row := range fake.ErrorCodes {
		status, published := liveStatus[row.Code]
		if !published {
			t.Errorf("schemaFixture claims error code %q, which the server does not publish", row.Code)
			continue
		}
		if status != row.Status {
			t.Errorf("error code %q: fake says HTTP %d, server says %d", row.Code, row.Status, status)
		}
	}
}

// TestContractEveryPublishedErrorCodeHasAnExitCode is the check that would have caught
// `no_fields` being absent from the exit-code map.
//
// It matters more than it looks. exitcode.FromErrorCode returns Unexpected for anything it
// does not know, so an unmapped code is not a crash — it is a 1 where the table promises
// something specific, which is exactly the kind of failure a script silently mishandles.
// Nothing in the unit suite can catch it, because the fakes only ever send codes someone
// already thought of.
func TestContractEveryPublishedErrorCodeHasAnExitCode(t *testing.T) {
	live := liveSchema(t)

	codes := live.ErrorCodeStrings()
	if len(codes) == 0 {
		t.Fatal("the server published no error codes; the schema cannot be right")
	}

	if unmapped := exitcode.Unmapped(codes); len(unmapped) > 0 {
		t.Errorf("the server publishes error codes this binary has no exit code for: %v\n\nAdd them to byCode in internal/exitcode/exitcode.go. Until then each one exits %d (unexpected)\nrather than the status the plan's table promises.",
			unmapped, exitcode.Unexpected)
	}
}

// ---------------------------------------------------------------------------
// 2. The fixture is rich enough to be worth seeding
// ---------------------------------------------------------------------------

// TestContractFixtureCoversEveryState guards the seed task rather than the CLI.
//
// M1 renders a share's state and M2 renders a field's status, and the golden files for
// that work are only meaningful if a real example of every value exists to render. A
// fixture that quietly stopped seeding, say, an expiring share would not fail anything —
// the goldens would just never exercise that branch. So the coverage is asserted here,
// while the fixture is cheap to change, instead of being discovered as a gap later.
func TestContractFixtureCoversEveryState(t *testing.T) {
	live := liveSchema(t)
	data := contractFixture(t)

	seededShareStates := map[string]bool{}
	for _, share := range data.Shares {
		seededShareStates[share.State] = true
	}
	for _, state := range live.ShareStates {
		if !seededShareStates[state] {
			t.Errorf("no seeded share is in state %q, so nothing can render it", state)
		}
	}

	seededFieldStatuses := map[string]bool{}
	for _, field := range data.Fields {
		seededFieldStatuses[field.Status] = true
	}
	for _, status := range live.FieldStatuses {
		if !seededFieldStatuses[status] {
			t.Errorf("no seeded field has status %q, so nothing can render it", status)
		}
	}

	// Every scope needs a token that has it and a token that does not, or the scope's
	// probe can only ever be tested in one direction.
	for _, scope := range live.Scopes {
		var withScope, withoutScope int
		for _, scopes := range activeTokens(data) {
			if slices.Contains(scopes, scope) {
				withScope++
			} else {
				withoutScope++
			}
		}
		if withScope == 0 {
			t.Errorf("no active token holds %q", scope)
		}
		if withoutScope == 0 {
			t.Errorf("every active token holds %q, so nothing can prove it is enforced", scope)
		}
	}
}

// TestContractDefaultFieldListingIsActiveOnly records a fact that is easy to get wrong
// and impossible to notice from a fake.
//
// GET /api/v1/fields with no status filter does not return the vault; it returns the
// active fields. The seed plants eight fields across four statuses and the unfiltered
// listing shows five. A `fields list` that labels that output "your fields" would be
// telling the holder their withdrawn and pending rows do not exist, so the asymmetry is
// pinned here before M1 builds on it.
func TestContractDefaultFieldListingIsActiveOnly(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, data.Tokens["all"].Raw)

	unfiltered := countFields(t, client, "")

	var active int
	for _, field := range data.Fields {
		if field.Status == "active" {
			active++
		}
	}

	if unfiltered != active {
		t.Errorf("the unfiltered listing returned %d fields; the fixture has %d active ones (of %d total).\n\nIf the default changed to return everything, `fields list` needs to say so and this test's premise is gone.",
			unfiltered, active, len(data.Fields))
	}
	if len(data.Fields) == active {
		t.Skip("the fixture only seeds active fields, so there is no asymmetry to detect")
	}

	// And each status is reachable when asked for by name, which is what --state will do.
	for _, status := range liveSchema(t).FieldStatuses {
		var expected int
		for _, field := range data.Fields {
			if field.Status == status {
				expected++
			}
		}
		if got := countFields(t, client, status); got != expected {
			t.Errorf("status=%s returned %d fields, fixture has %d", status, got, expected)
		}
	}
}

func countFields(t *testing.T, client *api.Client, status string) int {
	t.Helper()

	query := map[string]string{"limit": "200"}
	if status != "" {
		query["status"] = status
	}

	var body struct {
		Fields []struct {
			ID int `json:"id"`
		} `json:"fields"`
	}
	if err := getJSON(t, client, api.Prefix+"/fields", query, &body); err != nil {
		t.Fatalf("GET /fields (status=%q): %v", status, err)
	}
	return len(body.Fields)
}

func getJSON(t *testing.T, client *api.Client, path string, query map[string]string, into any) error {
	t.Helper()

	request := api.Request{Method: http.MethodGet, Path: path}
	if len(query) > 0 {
		request.Query = map[string][]string{}
		for key, value := range query {
			request.Query[key] = []string{value}
		}
	}

	response, err := client.Do(context.Background(), request)
	if err != nil {
		return err
	}
	return response.Decode(into)
}

// ---------------------------------------------------------------------------
// 3. The claim --probe rests on
// ---------------------------------------------------------------------------

// TestContractScopeIsCheckedBeforeTheBody arbitrates the one assumption in this CLI that
// its own doc comment admits is unverifiable locally.
//
// scopeProbes() detects the write scopes by sending a request that is designed to fail:
// an empty body for fields:write, a nonexistent field for documents:write, a missing
// Idempotency-Key for shares:mint. Reading "validation_failed" as "has the scope" is only
// sound if the API rejects on scope *before* it looks at the body. That ordering is
// documented but no endpoint exposes it, and if it ever inverted, --probe would report
// "lacks" for scopes a token holds — confidently, and with exit 0.
//
// So: for every active token and every probe, the code must be `insufficient_scope` when
// the token lacks the scope and the probe's own hasCode when it holds it. Anything else
// and --probe is lying.
func TestContractScopeIsCheckedBeforeTheBody(t *testing.T) {
	data := contractFixture(t)

	for handle, scopes := range activeTokens(data) {
		client := contractClient(t, data.Tokens[handle].Raw)

		for _, probe := range scopeProbes() {
			holdsScope := slices.Contains(scopes, probe.scope)

			_, err := client.Do(context.Background(), api.Request{
				Method: probe.method,
				Path:   probe.path,
				Query:  probe.query,
				Body:   probe.body,
			})

			var apiErr *api.Error
			if err != nil && !errors.As(err, &apiErr) {
				t.Fatalf("token %s, probe %s: transport failed: %v", handle, probe.scope, err)
			}

			switch {
			case !holdsScope:
				// The load-bearing half. A token without the scope must be refused on the
				// scope, not on the body it was never allowed to submit.
				if apiErr == nil {
					t.Errorf("token %s lacks %s but the probe succeeded — the endpoint is not enforcing scope, and the probe just created something",
						handle, probe.scope)
					continue
				}
				if apiErr.Code != "insufficient_scope" {
					t.Errorf("token %s lacks %s but the API answered %q, not insufficient_scope.\n\n--probe reads anything that is not insufficient_scope as \"has\", so it now reports this token holds a scope it does not.",
						handle, probe.scope, apiErr.Code)
				}

			case probe.hasCode == "":
				// A read probe: holding the scope means a 2xx.
				if apiErr != nil {
					t.Errorf("token %s holds %s but the read probe failed with %q", handle, probe.scope, apiErr.Code)
				}

			default:
				// A write probe: holding the scope means failing in the exact documented
				// way, which is also what proves nothing was written.
				if apiErr == nil {
					t.Errorf("token %s holds %s and the probe SUCCEEDED. It was supposed to fail with %q — it is no longer side-effect-free, and --probe is now a write.",
						handle, probe.scope, probe.hasCode)
					continue
				}
				if apiErr.Code != probe.hasCode {
					t.Errorf("token %s holds %s: expected %q, got %q.\n\n--probe will render this as \"unclear — %s\".",
						handle, probe.scope, probe.hasCode, apiErr.Code, apiErr.Code)
				}
			}
		}
	}
}

// TestContractProbesCreateNothing checks the other half of --probe's promise: the command
// prints "Nothing below creates or changes anything", and that is a claim about a live
// vault, made in the imperative. The test above proves each probe failed; this one proves
// the failures left no rows behind.
func TestContractProbesCreateNothing(t *testing.T) {
	data := contractFixture(t)
	client := contractClient(t, data.Tokens["all"].Raw)

	fieldsBefore := countFields(t, client, "")
	sharesBefore := countShares(t, client)

	// The full sweep, with the token that holds every scope — the case where each probe
	// gets far enough to do damage if it could.
	for _, probe := range scopeProbes() {
		_, _ = client.Do(context.Background(), api.Request{
			Method: probe.method,
			Path:   probe.path,
			Query:  probe.query,
			Body:   probe.body,
		})
	}

	if after := countFields(t, client, ""); after != fieldsBefore {
		t.Errorf("a --probe sweep changed the field count from %d to %d", fieldsBefore, after)
	}
	if after := countShares(t, client); after != sharesBefore {
		t.Errorf("a --probe sweep changed the share count from %d to %d — it minted something", sharesBefore, after)
	}
}

func countShares(t *testing.T, client *api.Client) int {
	t.Helper()

	var body struct {
		Shares []struct {
			Token string `json:"token"`
		} `json:"shares"`
	}
	if err := getJSON(t, client, api.Prefix+"/shares", map[string]string{"limit": "100"}, &body); err != nil {
		t.Fatalf("GET /shares: %v", err)
	}
	return len(body.Shares)
}

// ---------------------------------------------------------------------------
// 4. The commands, end to end
// ---------------------------------------------------------------------------

// TestContractWhoamiProbeMatchesTheFixture drives the real command against the real
// server and asserts the rendered verdicts, because the test above checks the API's
// answers and not what the CLI does with them.
func TestContractWhoamiProbeMatchesTheFixture(t *testing.T) {
	data := contractFixture(t)

	for _, handle := range []string{"all", "fields_read"} {
		token, seeded := data.Tokens[handle]
		if !seeded {
			t.Fatalf("the fixture has no %q token", handle)
		}

		harness := newHarness(t)
		t.Setenv("DOSSIER_TOKEN", token.Raw)

		if code := harness.run("whoami", "--probe", "--no-wait", "--host", contractHost(t)); code != exitcode.OK {
			t.Fatalf("%s: whoami --probe exited %d\nstdout:\n%s\nstderr:\n%s",
				handle, code, harness.stdout, harness.stderr)
		}

		output := harness.stdout.String()
		for _, probe := range scopeProbes() {
			want := "lacks"
			if slices.Contains(token.Scopes, probe.scope) {
				want = "has"
			}
			if !verdictReads(output, probe.scope, want) {
				t.Errorf("%s: expected %q to read %q. Full output:\n%s", handle, probe.scope, want, output)
			}
		}
	}
}

// verdictReads reports whether the rendered output has a line naming scope whose verdict
// is exactly want. Split into fields rather than searched as a substring: the block
// renderer pads the scope column, so the gap between name and verdict varies.
func verdictReads(output, scope, want string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == scope {
			return fields[1] == want
		}
	}
	return false
}

// TestContractLoginStoresAVerifiedToken runs login against the real server: the token
// shape check, the probe that proves it authenticates, and the 0600 write.
func TestContractLoginStoresAVerifiedToken(t *testing.T) {
	data := contractFixture(t)

	harness := newHarness(t)
	harness.stdinString(data.Tokens["all"].Raw + "\n")

	if code := harness.run("login", "--token-stdin", "--no-wait", "--host", contractHost(t)); code != exitcode.OK {
		t.Fatalf("login exited %d\nstdout:\n%s\nstderr:\n%s", code, harness.stdout, harness.stderr)
	}

	path := filepath.Join(harness.paths.ConfigDir, "credentials.toml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("login reported success but wrote no credentials file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("credentials.toml is mode %#o, want 0600", mode)
	}

	// The raw token must not appear in what login printed; only the masked prefix.
	if strings.Contains(harness.stdout.String(), data.Tokens["all"].Raw) {
		t.Error("login echoed the raw token to stdout")
	}
}

// TestContractDeadTokensAreIndistinguishable covers M0's done-when and rule 4's reasoning
// at once: a revoked token and an expired one must both fail, must fail identically, and
// must exit 3 rather than 0.
//
// Spends two units of the per-IP failed-authentication budget.
func TestContractDeadTokensAreIndistinguishable(t *testing.T) {
	data := contractFixture(t)

	outputs := map[string]string{}

	for _, handle := range []string{"revoked", "expired"} {
		token, seeded := data.Tokens[handle]
		if !seeded {
			t.Fatalf("the fixture has no %q token", handle)
		}

		harness := newHarness(t)
		t.Setenv("DOSSIER_TOKEN", token.Raw)

		code := harness.run("whoami", "--probe", "--no-wait", "--host", contractHost(t))
		if code == exitcode.TryLater {
			// Not a contract failure. The per-IP failed-authentication limiter is
			// already exhausted, which in practice means this suite was run inside the
			// last minute — the final test trips it on purpose. Said plainly, because
			// the alternative is reading this as the CLI mishandling a dead token.
			t.Fatalf("%s token was rate limited before it could be refused.\n\nThe per-IP failed-authentication budget is spent. The last test in this file\nexhausts it deliberately, so a re-run within a minute lands here. Wait a\nminute and run it again.", handle)
		}
		if code != exitcode.Unauthenticated {
			t.Errorf("%s token exited %d, want %d.\n\nA dead credential that exits 0 tells a script the check passed.\nstdout:\n%s\nstderr:\n%s",
				handle, code, exitcode.Unauthenticated, harness.stdout, harness.stderr)
		}
		outputs[handle] = harness.stderr.String()
	}

	// Rule 4's logic applied to tokens: the API does not distinguish expired from revoked
	// from never-valid, and the CLI's copy says so. If the two ever diverged here, one of
	// them would have become an oracle.
	if outputs["revoked"] != outputs["expired"] {
		t.Errorf("a revoked token and an expired one produced different output:\n--- revoked ---\n%s\n--- expired ---\n%s", outputs["revoked"], outputs["expired"])
	}
}

// ---------------------------------------------------------------------------
// 5. Last, because it trips a per-IP limiter
// ---------------------------------------------------------------------------

// TestContractRateLimitedCarriesRetryAfter must stay the final test in this file.
//
// It deliberately exhausts the failed-authentication limiter, which is keyed by IP and
// holds for a minute, so any test running after it that expects a 401 would see a 429
// instead. Go runs a file's tests in source order, which is what keeps this safe.
//
// What it buys: the client's entire retry policy is built on Retry-After being present
// and parseable on a 429. Every unit test of that path uses a header the test itself
// wrote, which proves the parser works and nothing about whether the server sends one.
func TestContractRateLimitedCarriesRetryAfter(t *testing.T) {
	data := contractFixture(t)

	// A syntactically valid token that was never minted. Using a malformed one risks
	// being rejected before the limiter is reached.
	client := contractClient(t, "dsk_"+strings.Repeat("A", 32))

	const attempts = 20
	var limited *api.Error

	for attempt := 1; attempt <= attempts && limited == nil; attempt++ {
		_, err := client.Do(context.Background(), api.Request{
			Method: http.MethodGet,
			Path:   api.Prefix + "/fields",
		})
		var apiErr *api.Error
		if err == nil {
			t.Fatal("a token that was never minted authenticated successfully")
		}
		if !errors.As(err, &apiErr) {
			t.Fatalf("transport failed: %v", err)
		}
		switch apiErr.Code {
		case "unauthenticated":
			// Expected, and counted against the limiter.
		case "rate_limited":
			limited = apiErr
		default:
			t.Fatalf("an unminted token produced %q, want unauthenticated or rate_limited", apiErr.Code)
		}
	}

	if limited == nil {
		t.Fatalf("%d failed authentications did not trip the limiter.\n\nThe schema advertises a limit on failed authentication per IP. If it is gone, the\nCLI's retry path is unreachable in production and `--no-wait` documents a limit\nthat is not enforced.", attempts)
	}

	if limited.Status != http.StatusTooManyRequests {
		t.Errorf("rate_limited arrived as HTTP %d, want 429", limited.Status)
	}
	if limited.RetryAfter <= 0 {
		t.Errorf("the 429 carried no usable Retry-After (got %d).\n\nWithout it the client cannot tell how long to wait, and its whole wait budget is\nguesswork.", limited.RetryAfter)
	}

	// And the counter is failure-only. The CLI relies on this without saying so: a
	// holder whose stored token is dead will retry, trip this limiter, and must still be
	// able to use a good token from the same machine. If the limiter counted every
	// request, one bad credential would lock the IP out of the API entirely.
	good := contractClient(t, data.Tokens["all"].Raw)
	if _, err := good.Do(context.Background(), api.Request{
		Method: http.MethodGet,
		Path:   api.Prefix + "/fields",
		Query:  map[string][]string{"limit": {"1"}},
	}); err != nil {
		t.Errorf("a valid token was refused while the IP was rate limited for failed authentication: %v\n\nThe limiter is counting successful requests too.", err)
	}
}
