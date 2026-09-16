package cli

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

const validToken = "dsk_BWBxo2sSPmLKdVcRTqHfZ3nWgY7jApNu"

// The shape check exists to catch a paste that lost a character *before* it spends one of
// the ten attempts per minute the per-IP failed-auth limiter allows. It is not a security
// check and never claims to be one.
func TestValidateTokenShape(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"a well-formed token", validToken, ""},
		{"empty", "", "No token"},
		{"no prefix", strings.TrimPrefix(validToken, "dsk_"), `begin with "dsk_"`},
		{"a web session cookie", "_dossier_session=abc123", `begin with "dsk_"`},
		{"truncated", "dsk_BWBxo2sS", "32"},
		{"too long", validToken + "XX", "32"},
		// 0, O, I and l are absent from base58 precisely so a transcribed token cannot be
		// ambiguous; their presence means a character was mangled in copying.
		{"contains a zero", "dsk_0WBxo2sSPmLKdVcRTqHfZ3nWgY7jApN", "32"},
		{"contains an ambiguous character", "dsk_OWBxo2sSPmLKdVcRTqHfZ3nWgY7jApNu", "never does"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTokenShape(tc.token)

			if tc.want == "" {
				if err != nil {
					t.Fatalf("a valid token was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q was accepted", tc.token)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			var cliErr *Error
			if !errorsAs(err, &cliErr) || cliErr.Code != exitcode.Usage {
				t.Errorf("a malformed token gave exit %v, want Usage (2)", err)
			}
		})
	}
}

// A token on the command line is written to the shell's history file and is visible in
// `ps` to every other user on the machine. Both refusals must be exit 2 — nothing was
// sent — and must say why.
func TestLoginRefusesATokenOnTheCommandLine(t *testing.T) {
	for _, args := range [][]string{
		{"login", validToken},
		{"login", "--token", validToken},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t)

			code := h.run(args...)

			if code != exitcode.Usage {
				t.Errorf("exit = %d, want %d", code, exitcode.Usage)
			}
			combined := h.stderr.String()
			if !strings.Contains(combined, "shell history") || !strings.Contains(combined, "ps") {
				t.Errorf("the refusal does not give the reason:\n%s", combined)
			}
			// Nothing may have been written.
			if _, err := os.Stat(h.paths.ConfigDir + "/credentials.toml"); err == nil {
				t.Error("a credentials file was created by a refused login")
			}
		})
	}
}

func TestLoginStoresAVerifiedToken(t *testing.T) {
	var probed bool
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": func(w http.ResponseWriter, r *http.Request) {
			probed = true
			if got := r.Header.Get("Authorization"); got != "Bearer "+validToken {
				t.Errorf("probe sent Authorization %q", got)
			}
			jsonResponse(http.StatusOK, `{"data":[],"next_after":null}`)(w, r)
		},
	})

	h := newHarness(t)
	h.stdinString(validToken + "\n")

	code := h.run("login", "--token-stdin", "--host", server.URL,
		"--scopes", "fields:read,shares:mint", "--expires-on", "2026-12-14", "--name", "laptop")

	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}
	if !probed {
		t.Error("login stored a token without verifying it")
	}

	creds, err := store.LoadCredentials(h.paths)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	profile, found := creds.Get("default")
	if !found {
		t.Fatal("no profile was stored")
	}
	if profile.Token != validToken {
		t.Errorf("stored token = %q", profile.Token)
	}
	if profile.Prefix != "BWBxo2sS" {
		t.Errorf("stored prefix = %q, want the first 8 characters after dsk_", profile.Prefix)
	}
	if profile.VerifiedAt != "2026-09-16T12:00:00Z" {
		t.Errorf("VerifiedAt = %q", profile.VerifiedAt)
	}
	if len(profile.Scopes) != 2 {
		t.Errorf("Scopes = %v", profile.Scopes)
	}

	// The token must never reach stdout. Only the prefix is shown, which is enough to
	// match the Settings row and useless to anyone reading over a shoulder.
	if strings.Contains(h.stdout.String(), validToken) {
		t.Errorf("the raw token was printed:\n%s", h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "dsk_BWBxo2sS…") {
		t.Errorf("the masked prefix was not shown:\n%s", h.stdout)
	}
}

// BaseController runs authenticate_api_token! ahead of every require_scope!, so a token
// lacking fields:read gets a 403, not a 401 — and a 403 proves the token is live. A
// read-only token minted for scripts lands here and must still be stored.
func TestLoginTreatsAScopeRefusalAsProofTheTokenIsLive(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": errorEnvelope(http.StatusForbidden, "insufficient_scope",
			"This token does not have the scope this endpoint requires.", "Mint a new token."),
	})

	h := newHarness(t)
	h.stdinString(validToken)

	code := h.run("login", "--token-stdin", "--host", server.URL)

	if code != 0 {
		t.Fatalf("exit = %d; a 403 means the token authenticates\nstderr: %s", code, h.stderr)
	}
	creds, _ := store.LoadCredentials(h.paths)
	if _, found := creds.Get("default"); !found {
		t.Error("a live token was not stored")
	}
	if !strings.Contains(h.stderr.String(), "no fields:read scope") {
		t.Errorf("the missing scope was not mentioned:\n%s", h.stderr)
	}
}

func TestLoginRefusesADeadToken(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": errorEnvelope(http.StatusUnauthorized, "unauthenticated",
			"Missing or invalid API token.", "Send a token that is not expired or revoked."),
	})

	h := newHarness(t)
	h.stdinString(validToken)

	code := h.run("login", "--token-stdin", "--host", server.URL)

	if code != exitcode.Unauthenticated {
		t.Fatalf("exit = %d, want %d", code, exitcode.Unauthenticated)
	}
	// §5.5's copy, not the envelope's: the API's hint is accurate but does not say that
	// the server will never tell you which of the three happened.
	if !strings.Contains(h.stderr.String(), "expired,") || !strings.Contains(h.stderr.String(), "same for all three") {
		t.Errorf("the §5.5 copy was not used:\n%s", h.stderr)
	}
	creds, _ := store.LoadCredentials(h.paths)
	if _, found := creds.Get("default"); found {
		t.Error("a dead token was stored")
	}
}

// A 429 proves *neither*: both the per-token read limiter and the per-IP failed-auth
// counter surface as rate_limited, so the probe did not run. Nothing may be stored, and
// the exit must be TryLater rather than a guess in either direction.
func TestLoginStoresNothingWhenTheProbeIsRateLimited(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": errorEnvelope(http.StatusTooManyRequests, "rate_limited",
			"Too many requests.", "Wait and try again."),
	})

	h := newHarness(t)
	h.stdinString(validToken)
	h.app.NoWait = true

	code := h.run("login", "--token-stdin", "--no-wait", "--host", server.URL)

	if code != exitcode.TryLater {
		t.Fatalf("exit = %d, want %d", code, exitcode.TryLater)
	}
	if !strings.Contains(h.stderr.String(), "did not run") {
		t.Errorf("the message does not say the check could not run:\n%s", h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "Nothing has been stored") {
		t.Errorf("the message does not say nothing was stored:\n%s", h.stderr)
	}

	creds, _ := store.LoadCredentials(h.paths)
	if _, found := creds.Get("default"); found {
		t.Error("a token was stored despite an unverifiable probe")
	}
}

// The schema is fetched before the probe, so a wrong --host fails having sent no
// credential anywhere.
func TestLoginSendsNoTokenToAHostThatIsNotADossier(t *testing.T) {
	var sawToken bool
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				sawToken = true
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<html>not a dossier</html>`))
		},
		"/api/v1/fields": func(w http.ResponseWriter, r *http.Request) {
			t.Error("the probe ran even though the schema fetch failed")
		},
	})

	h := newHarness(t)
	h.stdinString(validToken)

	if code := h.run("login", "--token-stdin", "--host", server.URL); code == 0 {
		t.Error("login succeeded against a host that is not a Dossier")
	}
	if sawToken {
		t.Error("the token was sent to the schema endpoint, which takes none")
	}
}

func TestLoginValidatesStatedScopesAgainstTheSchema(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": jsonResponse(http.StatusOK, `{"data":[]}`),
	})

	h := newHarness(t)
	h.stdinString(validToken)

	code := h.run("login", "--token-stdin", "--host", server.URL, "--scopes", "fields:read,shares:delete")

	if code != exitcode.Usage {
		t.Fatalf("exit = %d, want Usage", code)
	}
	if !strings.Contains(h.stderr.String(), "shares:delete") {
		t.Errorf("the unknown scope was not named:\n%s", h.stderr)
	}
}

func TestLoginValidatesTheStatedExpiryDate(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": jsonResponse(http.StatusOK, `{"data":[]}`),
	})

	h := newHarness(t)
	h.stdinString(validToken)

	code := h.run("login", "--token-stdin", "--host", server.URL, "--expires-on", "14/12/2026")

	if code != exitcode.Usage {
		t.Fatalf("exit = %d, want Usage", code)
	}
	if !strings.Contains(h.stderr.String(), "YYYY-MM-DD") {
		t.Errorf("the expected format was not given:\n%s", h.stderr)
	}
}

// A script that forgot --token-stdin must be told, not silently blocked on a prompt
// nobody will answer.
func TestLoginRefusesToPromptWhenStdinIsNotATerminal(t *testing.T) {
	h := newHarness(t)

	code := h.run("login")

	if code != exitcode.Usage {
		t.Fatalf("exit = %d, want Usage", code)
	}
	if !strings.Contains(h.stderr.String(), "--token-stdin") {
		t.Errorf("the alternative was not offered:\n%s", h.stderr)
	}
}

func TestLoginSetsTheDefaultProfileOnAFreshMachine(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": jsonResponse(http.StatusOK, `{"data":[]}`),
	})

	h := newHarness(t)
	h.stdinString(validToken)

	if code := h.run("login", "--token-stdin", "--host", server.URL, "--profile", "work"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	// A holder who logged in once should not then need --profile to use the only thing
	// they stored.
	config, err := store.LoadConfig(h.paths)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.DefaultProfile != "work" {
		t.Errorf("DefaultProfile = %q, want work", config.DefaultProfile)
	}
}

func TestSplitList(t *testing.T) {
	cases := map[string][]string{
		"a,b,c":      {"a", "b", "c"},
		"a b c":      {"a", "b", "c"},
		"a, b,  c  ": {"a", "b", "c"},
		"":           {},
		"   ":        {},
	}

	for input, want := range cases {
		got := splitList(input)
		if len(got) != len(want) {
			t.Errorf("splitList(%q) = %v, want %v", input, got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				t.Errorf("splitList(%q) = %v, want %v", input, got, want)
				break
			}
		}
	}
}

// errorsAs is a tiny local alias so the table test above reads cleanly.
func errorsAs(err error, target **Error) bool {
	cliErr, ok := err.(*Error)
	if ok {
		*target = cliErr
	}
	return ok
}
