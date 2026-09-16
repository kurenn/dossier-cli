package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kurenn/dossier-cli/internal/exitcode"
)

func TestErrorRenderKeepsTheAPICopyVerbatim(t *testing.T) {
	// The message and hint are the product's own copy, written for the person reading
	// them. A paraphrase is a defect, so they must survive rendering unaltered.
	message := "This token does not have the scope this endpoint requires."
	hint := "Mint a new token with the required scope from the Settings page — scopes are fixed at mint."

	apiErr := &Error{Code: "insufficient_scope", Message: message, Hint: hint, Status: http.StatusForbidden}
	out := apiErr.Render()

	if !strings.Contains(out, message) {
		t.Errorf("the message was altered:\n%s", out)
	}
	if !strings.Contains(out, hint) {
		t.Errorf("the hint was altered:\n%s", out)
	}
}

// §7.1: prose lives in sentences, data lives in labelled rows, and they never share a
// line. The code and status are data and get a label column; the message and hint are
// sentences and get their own lines.
func TestErrorRenderKeepsProseAndDataOnSeparateLines(t *testing.T) {
	apiErr := &Error{
		Code:    "not_found",
		Message: "Not found.",
		Hint:    "Check the id.",
		Status:  http.StatusNotFound,
	}

	for _, line := range strings.Split(apiErr.Render(), "\n") {
		if line == "" {
			continue
		}
		isDataRow := strings.HasPrefix(line, "CODE") || strings.HasPrefix(line, "STATUS")
		if !isDataRow {
			continue
		}
		// A data row must not also contain a sentence.
		if strings.Contains(line, "Not found.") || strings.Contains(line, "Check the id.") {
			t.Errorf("prose leaked into a data row: %q", line)
		}
	}

	if !strings.Contains(apiErr.Render(), "CODE") {
		t.Error("the code was not given a label column")
	}
}

// A code this binary has never heard of means the server is ahead of it. Saying so beats
// bucketing it into a plausible-looking exit that a script would then branch on
// confidently.
func TestErrorRenderSaysWhenACodeIsUnrecognised(t *testing.T) {
	apiErr := &Error{Code: "teapot_overflow", Message: "New failure.", Status: 418}
	out := apiErr.Render()

	if !strings.Contains(out, "does not recognise") {
		t.Errorf("an unknown code was rendered without explanation:\n%s", out)
	}
	if apiErr.ExitCode() != exitcode.Unexpected {
		t.Errorf("ExitCode = %d, want Unexpected", apiErr.ExitCode())
	}

	known := &Error{Code: "not_found", Message: "Not found.", Status: 404}
	if strings.Contains(known.Render(), "does not recognise") {
		t.Error("a known code was described as unrecognised")
	}
}

func TestErrorRenderHandlesAMissingEnvelope(t *testing.T) {
	apiErr := &Error{Status: http.StatusBadGateway}
	out := apiErr.Render()

	if !strings.Contains(out, "no error envelope") {
		t.Errorf("a missing envelope was not named:\n%s", out)
	}
	if !strings.Contains(apiErr.Error(), "502") {
		t.Errorf("Error() = %q, want the status", apiErr.Error())
	}
}

func TestErrorErrorString(t *testing.T) {
	apiErr := &Error{Code: "rate_limited", Message: "Too many requests."}

	if got := apiErr.Error(); got != "rate_limited: Too many requests." {
		t.Errorf("Error() = %q", got)
	}
}

func TestSchemaErrorCodeStrings(t *testing.T) {
	schema := &Schema{ErrorCodes: []ErrorCode{
		{Code: "not_found"},
		{Code: "rate_limited"},
	}}

	got := schema.ErrorCodeStrings()
	if len(got) != 2 || got[0] != "not_found" || got[1] != "rate_limited" {
		t.Errorf("ErrorCodeStrings() = %v", got)
	}
}
