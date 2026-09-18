package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/store"
)

// completeArgs drives cobra's completion machinery the way a shell does: the hidden
// `__complete` command, which is what every generated script actually calls.
//
// Going through it rather than calling the completion functions directly is the point.
// The functions being correct is worth little if they are never registered, and a
// registration that silently failed would leave `--profile <TAB>` falling back to a file
// listing with no test noticing.
func (h *harness) complete(args ...string) []string {
	h.t.Helper()

	h.run(append([]string{"__complete"}, args...)...)

	var suggestions []string
	for _, line := range strings.Split(h.stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		// The last line is the directive marker, e.g. ":4".
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		// Cobra appends a tab-separated description when a completion has one.
		suggestions = append(suggestions, strings.SplitN(line, "\t", 2)[0])
	}
	return suggestions
}

// TestCompletionNeverTouchesTheNetwork is the rule the whole file exists for.
//
// Completion fires on a keystroke. Against this API every request costs something real —
// a unit of a published budget, a row in the holder's audit trail, and on the recipient
// boundary an open that cannot be taken back. A shell that spent any of those while
// someone was still deciding what to type would be indefensible, and invisible: the
// completions would look perfectly normal.
//
// So this walks the whole command tree and asks for completions at every level, with a
// server wired up that fails the test if it is ever called.
func TestCompletionNeverTouchesTheNetwork(t *testing.T) {
	var called []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()

	h := newHarness(t)
	// A complete, working profile. The point is that completion does not use it even
	// when it could — an unauthenticated CLI not making requests proves nothing.
	h.seedProfile("work", store.Profile{Host: server.URL, Token: "dsk_" + strings.Repeat("a", 32)})
	h.seedDefaultProfile("work")

	invocations := [][]string{
		{""},
		{"--profile", ""},
		{"fields", ""},
		{"fields", "list", "--status", ""},
		{"fields", "create", "--category", ""},
		{"shares", ""},
		{"shares", "list", "--state", ""},
		{"shares", "mint", "--expires", ""},
		{"open", ""},
		{"documents", "attach", "--content-type", ""},
		{"whoami", ""},
		{"schema", ""},
		{"logout", ""},
	}

	for _, args := range invocations {
		h.reset()
		h.complete(args...)
	}

	if len(called) > 0 {
		t.Errorf("completion made %d request(s): %v\n\nA tab press must never spend a "+
			"rate-limit unit, write an audit row, or open a dossier.", len(called), called)
	}
}

// The closed vocabularies come from the cached schema, so completion agrees with the
// validation the command will apply a moment later.
func TestCompletionOffersTheSchemasVocabularies(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"field statuses", []string{"fields", "list", "--status", ""},
			[]string{"empty", "active", "pending", "withdrawn"}},
		{"field categories", []string{"fields", "create", "--category", ""},
			[]string{"identity", "travel", "tax", "health", "contact"}},
		{"share states", []string{"shares", "list", "--state", ""},
			[]string{"live", "expiring", "expired", "revoked"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedProfile("work", store.Profile{Host: "https://vault.example", Token: "dsk_x"})
			h.seedDefaultProfile("work")
			h.seedSchemaCache("https://vault.example")

			got := h.complete(testCase.args...)
			for _, want := range testCase.want {
				if !containsString(got, want) {
					t.Errorf("completion omits %q; got %v", want, got)
				}
			}
		})
	}
}

// A cold cache completes nothing. The alternative — fetching to fill it — is the one
// thing completion may not do, and a hardcoded fallback list would be this binary
// becoming a second source of truth for a vocabulary the schema publishes.
func TestCompletionOnAColdCacheOffersNothing(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("work", store.Profile{Host: "https://vault.example", Token: "dsk_x"})
	h.seedDefaultProfile("work")

	if got := h.complete("shares", "list", "--state", ""); len(got) != 0 {
		t.Errorf("a cold cache completed %v; it should offer nothing rather than guess", got)
	}
}

// Stale is fine here, where the commands refuse it. A completion list is a suggestion the
// server validates anyway, and refusing to complete because the cache turned 24 hours old
// would cost the feature to prevent one clear error message.
func TestCompletionUsesAStaleCache(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("work", store.Profile{Host: "https://vault.example", Token: "dsk_x"})
	h.seedDefaultProfile("work")
	h.seedSchemaCache("https://vault.example")

	// Well past the 24 h TTL the commands enforce.
	h.app.Now = func() time.Time { return fixedNow.Add(30 * 24 * time.Hour) }

	if got := h.complete("shares", "list", "--state", ""); !containsString(got, "live") {
		t.Errorf("a stale cache completed %v; staleness is the commands' problem, not completion's", got)
	}
}

func TestCompletionOffersStoredProfiles(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("work", store.Profile{Host: "https://vault.example", Token: "dsk_a"})
	h.seedProfile("personal", store.Profile{Host: "https://other.example", Token: "dsk_b"})

	got := h.complete("--profile", "")
	for _, want := range []string{"work", "personal"} {
		if !containsString(got, want) {
			t.Errorf("completion omits the stored profile %q; got %v", want, got)
		}
	}
}

// Nothing here enumerates a secret or a stranger's identifier. Dossier tokens and PINs
// belong to the recipient and are not ours to suggest; a file value is the shell's job.
func TestCompletionOffersNoSecrets(t *testing.T) {
	h := newHarness(t)
	h.seedProfile("work", store.Profile{Host: "https://vault.example", Token: "dsk_secret_token_value"})
	h.seedDefaultProfile("work")

	for _, args := range [][]string{
		{"open", ""},
		{"open", "K7M2P9QRX", "--document", ""},
		{"login", "--name", ""},
	} {
		h.reset()
		for _, suggestion := range h.complete(args...) {
			if strings.Contains(suggestion, "dsk_") {
				t.Errorf("completing %v suggested something token-shaped: %q", args, suggestion)
			}
		}
	}
}

// seedSchemaCache writes the fixture discovery document to the cache for a host.
func (h *harness) seedSchemaCache(host string) {
	h.t.Helper()

	if err := store.SaveSchema(h.paths, host, []byte(schemaFixture)); err != nil {
		h.t.Fatalf("SaveSchema: %v", err)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}
