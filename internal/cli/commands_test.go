package cli

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

func TestVersionGolden(t *testing.T) {
	h := newHarness(t)
	Version = "1.4.2"
	t.Cleanup(func() { Version = "dev" })

	if code := h.run("version"); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	// The Go version and platform vary by machine, so only the two stable rows are
	// asserted byte-exactly; a golden file over the whole block would fail on every
	// toolchain bump for no signal.
	out := h.stdout.String()
	if !strings.Contains(out, "DOSSIER    1.4.2") {
		t.Errorf("version row missing:\n%s", out)
	}
	if !strings.Contains(out, "API        /api/v1") {
		t.Errorf("API version row missing:\n%s", out)
	}
}

// `dossier` with no arguments must not exit 0 having printed help: that tells a script
// something succeeded.
func TestBareInvocationPrintsHelp(t *testing.T) {
	h := newHarness(t)

	if code := h.run(); code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(h.stdout.String()+h.stderr.String(), "dossier") {
		t.Error("no help was printed")
	}

	h.reset()
	if code := h.run("frobnicate"); code != exitcode.Usage {
		t.Errorf("an unknown command gave exit %d, want Usage", code)
	}
}

func TestSchemaGolden(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
	})

	h := newHarness(t)
	if code := h.run("schema", "--host", server.URL); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	// The host line varies per run, so it is normalised out. Everything else — the
	// vocabularies, the aligned limits, and the expiry block with the no-expiry exception
	// last and set apart — is asserted byte-exactly.
	got := strings.ReplaceAll(h.stdout.String(), server.URL, "https://dossier.example")
	assertGolden(t, "schema.txt", got)
}

// --json passes the API body through byte-for-byte. Re-serialising the decoded struct
// would publish a second contract that has to be kept in step with the first forever.
func TestSchemaJSONIsPassedThroughVerbatim(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
	})

	h := newHarness(t)
	if code := h.run("schema", "--json", "--host", server.URL); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	got := strings.TrimSuffix(h.stdout.String(), "\n")
	if got != schemaFixture {
		t.Errorf("--json altered the body:\n--- want ---\n%s\n--- got ---\n%s", schemaFixture, got)
	}
	// Nothing but the body on stdout, so `| jq` works without stripping a banner.
	if strings.Contains(got, "HOST") {
		t.Error("human output leaked into --json")
	}
}

func TestSchemaCachesAndRefreshes(t *testing.T) {
	var fetches int
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": func(w http.ResponseWriter, r *http.Request) {
			fetches++
			jsonResponse(http.StatusOK, schemaFixture)(w, r)
		},
	})

	h := newHarness(t)
	if code := h.run("schema", "--host", server.URL); code != 0 {
		t.Fatalf("first: exit = %d", code)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d after the first run", fetches)
	}

	h.reset()
	if code := h.run("schema", "--host", server.URL); code != 0 {
		t.Fatalf("second: exit = %d", code)
	}
	if fetches != 1 {
		t.Errorf("the cache was not used: fetches = %d", fetches)
	}
	if !strings.Contains(h.stderr.String(), "cache") {
		t.Errorf("the cached read was not disclosed:\n%s", h.stderr)
	}

	h.reset()
	if code := h.run("schema", "--refresh", "--host", server.URL); code != 0 {
		t.Fatalf("refresh: exit = %d", code)
	}
	if fetches != 2 {
		t.Errorf("--refresh did not bypass the cache: fetches = %d", fetches)
	}
}

// The one place the binary admits it may be behind the server, rather than letting an
// unknown code surface later as a mystery exit 1.
func TestSchemaReportsErrorCodesItCannotMap(t *testing.T) {
	withNewCode := strings.Replace(schemaFixture,
		`{"code":"rate_limited","status":429}`,
		`{"code":"rate_limited","status":429},{"code":"quantum_desync","status":418}`, 1)

	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, withNewCode),
	})

	h := newHarness(t)
	if code := h.run("schema", "--host", server.URL); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "quantum_desync") {
		t.Errorf("the unmappable code was not reported:\n%s", h.stderr)
	}
}

// `dossier schema` is the bare first run: it must work before any token or profile
// exists, because the endpoint takes no credential.
func TestSchemaWorksWithNoProfileAtAll(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
	})

	h := newHarness(t)
	if code := h.run("schema", "--host", server.URL); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}
	if _, err := os.Stat(h.paths.ConfigDir + "/credentials.toml"); err == nil {
		t.Error("schema created a credentials file")
	}
}

func TestWhoamiGolden(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{
		Host:       "https://dossier.example",
		Token:      validToken,
		Prefix:     "BWBxo2sS",
		Name:       "laptop",
		Scopes:     []string{"fields:read", "shares:mint"},
		ExpiresOn:  "2026-12-14",
		VerifiedAt: "2026-09-15T17:02:11Z",
	})

	if code := h.run("whoami"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	got := strings.ReplaceAll(h.stdout.String(), h.paths.ConfigDir, "/home/you/.config/dossier")
	assertGolden(t, "whoami.txt", got)
}

// The split between fact and statement is the whole design of this command. A holder's
// note about scopes in the same block as the host would read as something the server
// said.
func TestWhoamiSeparatesFactsFromStatements(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{
		Host:       "https://dossier.example",
		Token:      validToken,
		Scopes:     []string{"fields:read"},
		VerifiedAt: "2026-09-15T17:02:11Z",
	})

	if code := h.run("whoami"); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	out := h.stdout.String()
	headingAt := strings.Index(out, "Stated at login")
	scopesAt := strings.Index(out, "SCOPES")
	hostAt := strings.Index(out, "HOST")

	if headingAt < 0 {
		t.Fatalf("the statements heading is missing:\n%s", out)
	}
	if !(hostAt < headingAt && headingAt < scopesAt) {
		t.Errorf("scopes are not under the statements heading:\n%s", out)
	}
	if !strings.Contains(out, "does not report these") {
		t.Errorf("the heading does not disclaim the statements:\n%s", out)
	}
}

// The stored statements describe the *profile's* token. Under DOSSIER_TOKEN a different
// credential is in use, and printing them would make a --probe that disagreed look like
// the probe was broken rather than like the two were never related.
func TestWhoamiSuppressesStatementsForAnEnvironmentToken(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{
		Host:       "https://dossier.example",
		Token:      validToken,
		Scopes:     []string{"fields:read", "shares:mint"},
		ExpiresOn:  "2026-12-14",
		VerifiedAt: "2026-09-15T17:02:11Z",
	})
	t.Setenv("DOSSIER_TOKEN", "dsk_ZZZxo2sSPmLKdVcRTqHfZ3nWgY7jApNu")

	if code := h.run("whoami"); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	out := h.stdout.String()
	if strings.Contains(out, "shares:mint") || strings.Contains(out, "2026-12-14") {
		t.Errorf("the stored statements were shown for an environment token:\n%s", out)
	}
	// Nor the stored verification, which recorded a check on a different string.
	if strings.Contains(out, "2026-09-15T17:02:11Z") {
		t.Errorf("the stored verification time was shown for an environment token:\n%s", out)
	}
	if !strings.Contains(out, "DOSSIER_TOKEN") {
		t.Errorf("the source of the token was not disclosed:\n%s", out)
	}
}

func TestWhoamiNeedsAToken(t *testing.T) {
	h := newHarness(t)

	code := h.run("whoami")

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want Usage", code)
	}
	if !strings.Contains(h.stderr.String(), "dossier login") {
		t.Errorf("the remedy was not given:\n%s", h.stderr)
	}
}

// Each probe is chosen so a token that *does* have the scope still changes nothing, which
// is what makes --probe safe against a live vault.
func TestWhoamiProbeReportsEachScope(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/fields":             jsonResponse(http.StatusOK, `{"data":[]}`),
		"/api/v1/shares":             sharesProbeHandler(t),
		"/api/v1/fields/0/documents": errorEnvelope(http.StatusForbidden, "insufficient_scope", "No.", "Mint another."),
	})

	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: validToken})

	if code := h.run("whoami", "--probe"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	out := h.stdout.String()
	for _, want := range []string{
		"fields:read       has",
		"shares:read       has",
		"shares:mint       has",
		"documents:write   lacks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// sharesProbeHandler answers the read probe with a 200 and the mint probe — a POST with
// no Idempotency-Key — with the 422 the API really sends, which is what proves the scope
// without claiming an idempotency row.
func sharesProbeHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			jsonResponse(http.StatusOK, `{"data":[]}`)(w, r)
			return
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("the mint probe sent an Idempotency-Key; it must not claim a row")
		}
		errorEnvelope(http.StatusUnprocessableEntity, "idempotency_key_required",
			"Idempotency-Key is required.", "Send one.")(w, r)
	}
}

// M0's done-when: a revoked token produces exit 3 and the §5.5 message. Reporting it as a
// scope result five times and exiting 0 would tell a script the check succeeded.
func TestWhoamiProbeStopsOnADeadToken(t *testing.T) {
	var requests int
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/fields": func(w http.ResponseWriter, r *http.Request) {
			requests++
			errorEnvelope(http.StatusUnauthorized, "unauthenticated",
				"Missing or invalid API token.", "Send a live token.")(w, r)
		},
	})

	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: validToken})

	code := h.run("whoami", "--probe")

	if code != exitcode.Unauthenticated {
		t.Fatalf("exit = %d, want %d", code, exitcode.Unauthenticated)
	}
	if requests != 1 {
		t.Errorf("made %d requests after the token proved dead, want 1", requests)
	}
	if !strings.Contains(h.stderr.String(), "same for all three") {
		t.Errorf("the §5.5 copy was not used:\n%s", h.stderr)
	}
}

// The same argument as the test above, for the other non-answer. A sweep the rate limiter
// refused determined nothing, so it must not exit 0 — found by the contract suite, which
// unlike a fake can actually exhaust a real limiter.
//
// The verdicts still print: the probes that did run are real results, and the exit code
// is what tells a caller the set is incomplete.
func TestWhoamiProbeExitsTryLaterWhenRateLimited(t *testing.T) {
	rateLimited := errorEnvelope(http.StatusTooManyRequests, "rate_limited",
		"Too many requests.", "Wait a minute.")

	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/fields": func(w http.ResponseWriter, r *http.Request) {
			// The read probe answers, so some of the sweep is real.
			if r.Method == http.MethodGet {
				jsonResponse(http.StatusOK, `{"data":[]}`)(w, r)
				return
			}
			rateLimited(w, r)
		},
		"/api/v1/shares":             rateLimited,
		"/api/v1/fields/0/documents": rateLimited,
	})

	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: validToken})

	code := h.run("whoami", "--probe", "--no-wait")

	if code != exitcode.TryLater {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitcode.TryLater, h.stdout, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "fields:read       has") {
		t.Errorf("the probe that did answer was not reported:\n%s", out)
	}
	if !strings.Contains(h.stdout.String(), verdictRateLimited) {
		t.Errorf("the refused probes were not marked as unknown:\n%s", h.stdout)
	}
	if !strings.Contains(h.stderr.String(), "it is incomplete") {
		t.Errorf("stderr does not say the sweep was incomplete:\n%s", h.stderr)
	}
}

// The stated expiry is the holder's note, never presented as the cause of a 401 — the
// CLI cannot know a token's real expiry and the stored date may simply be wrong.
func TestUnauthenticatedMentionsAStatedExpiryAsAStatement(t *testing.T) {
	passed := unauthenticated(store.Profile{ExpiresOn: "2026-01-01"}, fixedNow, false)
	if !strings.Contains(passed.Message, "At login you said") {
		t.Errorf("a passed stated expiry was not mentioned:\n%s", passed.Message)
	}

	future := unauthenticated(store.Profile{ExpiresOn: "2027-01-01"}, fixedNow, false)
	if strings.Contains(future.Message, "At login you said") {
		t.Errorf("a future stated expiry was mentioned:\n%s", future.Message)
	}

	// Under DOSSIER_TOKEN the stored date describes a different token.
	fromEnv := unauthenticated(store.Profile{ExpiresOn: "2026-01-01"}, fixedNow, true)
	if strings.Contains(fromEnv.Message, "At login you said") {
		t.Errorf("a stored date was attributed to an environment token:\n%s", fromEnv.Message)
	}

	if passed.Code != exitcode.Unauthenticated {
		t.Errorf("Code = %d", passed.Code)
	}
}

// A 401 must never delete the stored token: the holder may be offline or on the wrong
// host, and nothing in this program can mint a replacement.
func TestADeadTokenIsNotDeleted(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/fields": errorEnvelope(http.StatusUnauthorized, "unauthenticated", "No.", "Mint another."),
	})

	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: server.URL, Token: validToken})

	if code := h.run("whoami", "--probe"); code != exitcode.Unauthenticated {
		t.Fatalf("exit = %d", code)
	}

	creds, err := store.LoadCredentials(h.paths)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	profile, found := creds.Get("default")
	if !found || profile.Token != validToken {
		t.Error("the stored token was destroyed by a 401")
	}
}

func TestProfilesListGolden(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{
		Host: "https://dossier.example", Prefix: "BWBxo2sS", ExpiresOn: "2026-12-14", Name: "laptop",
	})
	h.seedProfile("scripts", store.Profile{
		Host: "https://dossier.example", Prefix: "QK3mZp9T", Name: "read-only for cron",
	})
	h.seedDefaultProfile("default")

	if code := h.run("profiles", "list"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}
	assertGolden(t, "profiles-list.txt", h.stdout.String())
}

func TestProfilesUse(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: "https://a.example", Token: validToken})
	h.seedProfile("work", store.Profile{Host: "https://b.example", Token: validToken})

	if code := h.run("profiles", "use", "work"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}
	config, err := store.LoadConfig(h.paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q", config.DefaultProfile)
	}

	h.reset()
	code := h.run("profiles", "use", "nonexistent")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want Usage", code)
	}
	// The refusal lists what is actually there, so the holder does not have to guess.
	if !strings.Contains(h.stderr.String(), "work") {
		t.Errorf("the stored profiles were not listed:\n%s", h.stderr)
	}
}

func TestProfilesListWithNothingStored(t *testing.T) {
	h := newHarness(t)

	if code := h.run("profiles", "list"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "dossier login") {
		t.Errorf("the next step was not offered:\n%s", h.stderr)
	}
}

// The whole point of logout's copy: a holder who ran it because a token leaked has not
// fixed anything yet, and this is the only moment they are certain to be reading.
func TestLogoutSaysItDidNotRevoke(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: "https://dossier.example", Token: validToken})

	if code := h.run("logout"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	creds, _ := store.LoadCredentials(h.paths)
	if _, found := creds.Get("default"); found {
		t.Error("the token was not forgotten")
	}

	out := h.stderr.String()
	if !strings.Contains(out, "did not revoke") {
		t.Errorf("logout did not say it does not revoke:\n%s", out)
	}
	if !strings.Contains(out, "API tokens") {
		t.Errorf("logout did not say where to revoke:\n%s", out)
	}
}

// The end state the holder asked for is "this machine has no token for that profile", and
// it is already true — failing would make logout unsafe in a teardown script.
func TestLogoutOnAnEmptyProfileSucceeds(t *testing.T) {
	h := newHarness(t)

	if code := h.run("logout"); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(h.stderr.String(), "nothing to forget") {
		t.Errorf("logout was not explicit about the no-op:\n%s", h.stderr)
	}
}

func TestLogoutWarnsThatDOSSIERTOKENStillWins(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: "https://dossier.example", Token: validToken})
	t.Setenv("DOSSIER_TOKEN", "dsk_ZZZxo2sSPmLKdVcRTqHfZ3nWgY7jApNu")

	if code := h.run("logout"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// Otherwise the holder believes they have disconnected and the next command quietly
	// authenticates anyway.
	if !strings.Contains(h.stderr.String(), "DOSSIER_TOKEN is still set") {
		t.Errorf("the environment override was not mentioned:\n%s", h.stderr)
	}
}

// The refusal must reach every command that reads the file, not just whoami, and it must
// be exit 2 — the CLI refused before sending anything.
func TestAWorldReadableCredentialsFileIsRefusedEverywhere(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{Host: "https://dossier.example", Token: validToken})

	if err := os.Chmod(h.paths.ConfigDir+"/credentials.toml", 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	for _, args := range [][]string{{"whoami"}, {"logout"}, {"profiles", "list"}, {"profiles", "use", "default"}} {
		h.reset()

		code := h.run(args...)

		if code != exitcode.Usage {
			t.Errorf("%v gave exit %d, want Usage", args, code)
		}
		if !strings.Contains(h.stderr.String(), "chmod 600") {
			t.Errorf("%v did not name the remedy:\n%s", args, h.stderr)
		}
	}
}

// --quiet suppresses the prose about the run, but never the data. A script asking for
// quiet still wants its answer.
func TestQuietSuppressesProseNotData(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("default", store.Profile{
		Host: "https://dossier.example", Token: validToken, Prefix: "BWBxo2sS",
	})

	if code := h.run("whoami", "--quiet"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "dsk_BWBxo2sS") {
		t.Errorf("--quiet suppressed the data block:\n%s", h.stdout)
	}
}
