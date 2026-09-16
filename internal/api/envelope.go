package api

import (
	"fmt"
	"strings"

	"github.com/kurenn/dossier-cli/internal/exitcode"
)

// Error is a Dossier API error envelope.
//
// The API answers every failure with the same shape — `{"error": {"code", "message",
// "hint"}}` — which is what lets this one type cover the whole surface. `message` and
// `hint` are rendered verbatim wherever they are shown: they are the product's copy,
// written for the person reading them, and a paraphrase is a defect (CLAUDE.md).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`

	// Status is the HTTP status that carried the envelope. Kept because two envelopes
	// with the same code can arrive on different statuses in one case that matters — a
	// 409 request_in_flight before a 5xx means "wait", after one means "go look" — and
	// because an unrecognised code is worth reporting with the status that carried it.
	Status int `json:"-"`

	// RetryAfter is the parsed Retry-After header in seconds, zero if absent. The server
	// decides the wait; the CLI never invents one.
	RetryAfter int `json:"-"`

	// Body is the raw response bytes, kept so --json can pass the envelope through to
	// stderr byte-for-byte rather than re-serialising a decoded struct into a shape the
	// API never sent.
	Body []byte `json:"-"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d with no error envelope", e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ExitCode maps this envelope onto the CLI's status contract.
//
// An unrecognised code deliberately returns Unexpected rather than guessing from the
// status class. A code this binary has never heard of means the server is ahead of it,
// and bucketing that into a plausible-looking exit would let a script branch confidently
// on a decision nobody made.
func (e *Error) ExitCode() int {
	if exit, known := exitcode.FromErrorCode(e.Code); known {
		return exit
	}
	return exitcode.Unexpected
}

// Known reports whether this binary recognises the envelope's code.
func (e *Error) Known() bool {
	_, known := exitcode.FromErrorCode(e.Code)
	return known
}

// envelope is the wire shape. Separate from Error so the exported type can carry the
// transport details above without them looking like fields the server sent.
type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

// Render returns the envelope as the CLI prints it to stderr: the message, then the hint
// on its own line, both verbatim, then the code in the label column so a human reading a
// terminal and a human reading a bug report are looking at the same string.
//
// Prose and data do not share a line here, per the plan's §7.1 — the message and hint are
// sentences, the code is a datum, and the code gets a label column of its own.
func (e *Error) Render() string {
	var out strings.Builder
	if e.Message != "" {
		out.WriteString(e.Message)
		out.WriteString("\n")
	}
	if e.Hint != "" {
		out.WriteString(e.Hint)
		out.WriteString("\n")
	}
	if e.Message != "" || e.Hint != "" {
		out.WriteString("\n")
	}

	code := e.Code
	if code == "" {
		code = "(no error envelope)"
	}
	fmt.Fprintf(&out, "%-12s %s\n", "CODE", code)
	fmt.Fprintf(&out, "%-12s %d\n", "STATUS", e.Status)

	if !e.Known() && e.Code != "" {
		out.WriteString("\nThis version of dossier does not recognise that error code, so it " +
			"cannot map it to\na meaningful exit status. The server is likely newer than this " +
			"binary.\n")
	}
	return out.String()
}
