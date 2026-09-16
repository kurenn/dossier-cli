package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// interactiveHarness drives the prompting paths. IsInteractive is injected rather than
// allocating a pty; stdin stays a plain reader, so readSecret takes its non-terminal
// branch and the answers are read as lines.
func interactiveHarness(t *testing.T, answers string) *harness {
	t.Helper()

	h := newHarness(t)
	h.app.IsInteractive = func() bool { return true }
	h.stdinString(answers)
	return h
}

func interactiveServer(t *testing.T) string {
	t.Helper()

	return fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
		"/api/v1/fields": jsonResponse(http.StatusOK, `{"data":[],"next_after":null}`),
	}).URL
}

// The regression this covers: a scanner per prompt reads ahead into its own buffer, so
// the second answer vanishes. Three prompts in sequence is the whole interactive login.
func TestInteractiveLoginReadsEveryAnswer(t *testing.T) {
	h := interactiveHarness(t, validToken+"\n1 4\n2026-12-14\n")

	if code := h.run("login", "--host", interactiveServer(t)); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
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
		t.Errorf("token = %q", profile.Token)
	}
	// Choices 1 and 4 against the fixture's scope list.
	if strings.Join(profile.Scopes, " ") != "fields:read shares:mint" {
		t.Errorf("scopes = %v, want the two chosen by number", profile.Scopes)
	}
	if profile.ExpiresOn != "2026-12-14" {
		t.Errorf("expires_on = %q; the third prompt's answer was lost", profile.ExpiresOn)
	}
}

// Both statements are optional: the API will not confirm either, so refusing to proceed
// without them would be demanding a guess for no benefit.
func TestInteractiveLoginAcceptsSkippedStatements(t *testing.T) {
	h := interactiveHarness(t, validToken+"\n\n\n")

	if code := h.run("login", "--host", interactiveServer(t)); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, h.stderr)
	}

	creds, _ := store.LoadCredentials(h.paths)
	profile, _ := creds.Get("default")
	if len(profile.Scopes) != 0 || profile.ExpiresOn != "" {
		t.Errorf("skipped prompts recorded something: %+v", profile)
	}
}

// The preface is the one thing that stops `login` being read as "authenticate me", which
// it is not and cannot be — the API has no token-management surface at all.
func TestInteractiveLoginPrintsThePreface(t *testing.T) {
	h := interactiveHarness(t, validToken+"\n\n\n")

	if code := h.run("login", "--host", interactiveServer(t)); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	out := h.stderr.String()
	if !strings.Contains(out, "It cannot mint one") {
		t.Errorf("the preface does not disclaim minting:\n%s", out)
	}
	if !strings.Contains(out, "Settings") {
		t.Errorf("the preface does not say where a token comes from:\n%s", out)
	}
	// The preface belongs on stderr with the rest of the prose about the run, so a script
	// capturing stdout gets only the data block.
	if strings.Contains(h.stdout.String(), "cannot mint") {
		t.Errorf("the preface leaked onto stdout:\n%s", h.stdout)
	}
}

func TestInteractiveLoginRejectsAScopeNumberOutOfRange(t *testing.T) {
	for _, answer := range []string{"9", "0", "abc"} {
		h := interactiveHarness(t, validToken+"\n"+answer+"\n")

		code := h.run("login", "--host", interactiveServer(t))

		if code != exitcode.Usage {
			t.Errorf("answer %q gave exit %d, want Usage", answer, code)
		}
	}
}

func TestInteractiveLoginRejectsAMalformedExpiryDate(t *testing.T) {
	h := interactiveHarness(t, validToken+"\n\nnot-a-date\n")

	if code := h.run("login", "--host", interactiveServer(t)); code != exitcode.Usage {
		t.Errorf("exit = %d, want Usage", code)
	}
	if !strings.Contains(h.stderr.String(), "YYYY-MM-DD") {
		t.Errorf("the expected format was not given:\n%s", h.stderr)
	}
}

// The scope checklist is built from the schema's own list, never a hardcoded one, so a
// scope the API adds appears without this client changing.
func TestInteractiveLoginBuildsTheChecklistFromTheSchema(t *testing.T) {
	h := interactiveHarness(t, validToken+"\n\n\n")

	if code := h.run("login", "--host", interactiveServer(t)); code != 0 {
		t.Fatalf("exit = %d", code)
	}

	out := h.stderr.String()
	for _, scope := range []string{"fields:read", "fields:write", "documents:write", "shares:mint", "shares:read"} {
		if !strings.Contains(out, scope) {
			t.Errorf("the checklist omitted %q:\n%s", scope, out)
		}
	}
	if !strings.Contains(out, "Dossier cannot tell us") {
		t.Errorf("the checklist does not say the answer is unverifiable:\n%s", out)
	}
}

// An empty stdin at the token prompt is a usage error, not a crash or an empty token
// stored.
func TestLoginOnEmptyStdin(t *testing.T) {
	h := interactiveHarness(t, "")

	if code := h.run("login", "--host", interactiveServer(t)); code != exitcode.Usage {
		t.Errorf("exit = %d, want Usage", code)
	}
}

func TestReadLineTreatsEndOfInputAsSkip(t *testing.T) {
	h := newHarness(t)
	h.stdinString("")

	got, err := h.app.readLine("anything? ")
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "" {
		t.Errorf("readLine = %q", got)
	}
}
