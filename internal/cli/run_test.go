package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// The exit contract end to end, driven the way the process drives it.
func TestRunContextReturnsTheExitCode(t *testing.T) {
	server := fakeAPI(t, map[string]http.HandlerFunc{
		"/api/v1/schema": jsonResponse(http.StatusOK, schemaFixture),
	})

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"a successful command", []string{"schema", "--host", server.URL}, exitcode.OK},
		{"an unknown command", []string{"frobnicate"}, exitcode.Usage},
		{"an unknown flag", []string{"schema", "--nope"}, exitcode.Usage},
		{"a command needing a token with none stored", []string{"whoami"}, exitcode.Usage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOSSIER_TOKEN", "")
			t.Setenv("DOSSIER_PROFILE", "")

			var stdout, stderr bytes.Buffer
			streams := Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}

			got := RunContext(context.Background(), tc.args, streams, store.PathsIn(t.TempDir()), time.Now)

			if got != tc.want {
				t.Errorf("exit = %d, want %d\nstderr: %s", got, tc.want, stderr.String())
			}
		})
	}
}

// A message that already reads as a sentence does not need an "Error:" label, and the
// exit code is the machine-readable half of the answer.
func TestReportPrintsVerbatimAndReturnsTheCode(t *testing.T) {
	var stderr bytes.Buffer

	code := Report(&stderr, &Error{Code: exitcode.ShareClosed, Message: "This share was revoked.\n"})

	if code != exitcode.ShareClosed {
		t.Errorf("code = %d", code)
	}
	if got := stderr.String(); got != "This share was revoked.\n" {
		t.Errorf("stderr = %q; nothing may be added to the message", got)
	}
}

func TestReportAddsAMissingNewline(t *testing.T) {
	var stderr bytes.Buffer

	Report(&stderr, &Error{Code: exitcode.Usage, Message: "No trailing newline"})

	if !strings.HasSuffix(stderr.String(), "\n") {
		t.Errorf("stderr = %q, want a trailing newline", stderr.String())
	}
}

// An error from outside this package — a cobra failure, a bug — must still produce a
// code rather than panicking or exiting 0.
func TestReportClassifiesAForeignError(t *testing.T) {
	var stderr bytes.Buffer

	code := Report(&stderr, errors.New("something unexpected"))

	if code != exitcode.Unexpected {
		t.Errorf("code = %d, want Unexpected", code)
	}
	if !strings.Contains(stderr.String(), "something unexpected") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestClassify(t *testing.T) {
	t.Run("an API envelope keeps its mapped code", func(t *testing.T) {
		got := Classify(&api.Error{Code: "share_expired", Message: "Gone.", Status: 410})
		if got.Code != exitcode.ShareClosed {
			t.Errorf("Code = %d, want ShareClosed", got.Code)
		}
	})

	t.Run("a CLI error passes through unchanged", func(t *testing.T) {
		original := Usagef("nope\n")
		if got := Classify(original); got != original {
			t.Error("a *cli.Error was re-wrapped")
		}
	})

	t.Run("anything else is unexpected", func(t *testing.T) {
		if got := Classify(errors.New("boom")); got.Code != exitcode.Unexpected {
			t.Errorf("Code = %d", got.Code)
		}
	})
}

func TestErrorUnwrapAndMessage(t *testing.T) {
	inner := errors.New("root cause")
	wrapped := &Error{Code: exitcode.Unexpected, Err: inner}

	if !errors.Is(wrapped, inner) {
		t.Error("Unwrap did not expose the cause")
	}
	// With no message, Error() falls back to the cause rather than an empty string.
	if wrapped.Error() != "root cause" {
		t.Errorf("Error() = %q", wrapped.Error())
	}

	bare := &Error{Code: exitcode.TryLater}
	if !strings.Contains(bare.Error(), "8") {
		t.Errorf("Error() = %q, want the code", bare.Error())
	}
}

func TestFromAPIErrorKeepsTheEnvelope(t *testing.T) {
	apiErr := &api.Error{Code: "not_found", Message: "Not found.", Hint: "Check the id.", Status: 404}

	got := FromAPIError(apiErr)

	if got.Code != exitcode.NotFound {
		t.Errorf("Code = %d", got.Code)
	}
	if !strings.Contains(got.Message, "Not found.") || !strings.Contains(got.Message, "Check the id.") {
		t.Errorf("the envelope copy was lost:\n%s", got.Message)
	}
	if !errors.Is(got.Err, error(apiErr)) {
		t.Error("the underlying envelope was not retained")
	}
}
