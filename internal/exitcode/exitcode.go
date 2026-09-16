// Package exitcode is the CLI's status contract with whatever ran it.
//
// The plan's §4.3 chose small contiguous codes over sysexits.h deliberately: sysexits
// has no code for "gone", "locked" or "conflict", and mapping HTTP 410 onto
// EX_UNAVAILABLE would make a script treat a deliberately revoked share like a network
// outage. These numbers are the contract — a shell `case` on $? and an agent branching
// on a tool call both depend on them, so they may be added to but never renumbered.
package exitcode

import "slices"

// The codes, verbatim from the plan's table. Success is 0 and unexpected is 1 so that a
// panic, a `go` runtime failure or a killed process all land on "unexpected" without any
// help from us.
const (
	// OK is a 2xx, or an idempotent replay of one.
	OK = 0

	// Unexpected covers transport failure, an unparseable response, and CLI bugs:
	// connection refused, TLS failure, a timeout, an unretried 5xx, a non-envelope body.
	Unexpected = 1

	// Usage means the CLI refused before sending anything — a missing expiry flag, an
	// unknown category, an unreadable file, a token passed as an argument. Nothing
	// reached the network, so nothing needs reconciling.
	Usage = 2

	// Unauthenticated is 401 unauthenticated. The API makes expired, revoked and never-valid
	// indistinguishable on purpose, so this one code covers all three (§5.5).
	Unauthenticated = 3

	// ScopeMissing is 403 insufficient_scope. Distinct from Unauthenticated because the
	// remedy is different and a script can act on it: the token is real, it just cannot
	// do this. Scopes are fixed at mint, so the remedy is a new token, never a re-login.
	ScopeMissing = 4

	// NotFound is 404 not_found — or not yours. The API does not distinguish, and neither
	// does this.
	NotFound = 5

	// Invalid is the request refused as malformed or unprocessable: 400 bad_request,
	// 415 unsupported_media_type, and every 422.
	Invalid = 6

	// IdempotencyConflict is 409 idempotency_key_reused: the same key with different
	// bytes. Not retryable by definition — retrying cannot make the bytes match a claim
	// that already exists — which is why it is separate from TryLater.
	IdempotencyConflict = 7

	// TryLater is 429 rate_limited once the wait budget is spent or under --no-wait, and
	// 409 request_in_flight after retries. The distinguishing property is that nothing
	// was written, so repeating the same command later is safe.
	TryLater = 8

	// ShareClosed is 410 share_expired or 410 share_revoked. A deadline that arrived or a
	// holder who revoked — both are the product working, not a failure, which is why this
	// is not folded into NotFound.
	ShareClosed = 9

	// PinRefused is 401 pin_required, 401 pin_incorrect and 423 pin_locked. Separate from
	// Unauthenticated because the credential in question is the recipient's PIN, not a
	// bearer token, and the remedy involves a different person entirely.
	PinRefused = 10

	// MintUnconfirmed is the one outcome where doing nothing is safer than retrying: a 5xx
	// on mint, retried once with the same key, answered 409 request_in_flight. The batch
	// probably exists; the API cannot confirm it, and the CLI will not guess either way.
	//
	// It has its own code so a script can branch on "go look" without parsing prose, and
	// so it can never be confused with TryLater — where repeating the command is safe. Here
	// it is the one thing that might mint twice.
	MintUnconfirmed = 11
)

// FromErrorCode maps an API error envelope's `code` to an exit code.
//
// The mapping is driven by the envelope's string rather than the HTTP status because the
// API's own reference documents the codes as the stable contract and the statuses as a
// consequence of them. Two codes share a status (401 covers `unauthenticated`,
// `pin_required` and `pin_incorrect`) and land on different exits, so switching on status
// could not express this table at all.
//
// An unrecognised code returns Unexpected and false. The caller decides whether that is
// fatal: a code this CLI has never heard of means the server is newer than the binary,
// which is worth saying out loud rather than silently bucketing as a generic failure.
func FromErrorCode(code string) (int, bool) {
	if exit, known := byCode[code]; known {
		return exit, true
	}
	// Unexpected, emphatically not the map's zero value — which is 0, and 0 is success.
	// A caller that ignored the boolean would otherwise report a failure it did not
	// understand as a clean run.
	return Unexpected, false
}

// All nineteen codes in the API's own table. Six of them share the Invalid exit: they are
// all "the server understood the request and refused it as unprocessable", and a script
// that wants to tell `file_too_large` from `expiry_required` reads the envelope's code,
// which --json passes through untouched. Collapsing them here is what keeps the exit
// contract small enough to be worth switching on.
var byCode = map[string]int{
	"unauthenticated":    Unauthenticated,
	"insufficient_scope": ScopeMissing,
	"not_found":          NotFound,

	"bad_request":              Invalid,
	"unsupported_media_type":   Invalid,
	"validation_failed":        Invalid,
	"expiry_required":          Invalid,
	"no_fields":                Invalid,
	"file_too_large":           Invalid,
	"vault_full":               Invalid,
	"idempotency_key_required": Invalid,

	"idempotency_key_reused": IdempotencyConflict,

	"rate_limited":      TryLater,
	"request_in_flight": TryLater,

	"share_expired": ShareClosed,
	"share_revoked": ShareClosed,

	"pin_required":  PinRefused,
	"pin_incorrect": PinRefused,
	"pin_locked":    PinRefused,
}

// Unmapped returns the codes the given schema publishes that this binary has no exit code
// for, sorted.
//
// The schema's `error_codes` is the authority on what the server can answer, and it is
// fetched at runtime, so a server newer than the binary is a state the CLI can actually
// detect rather than guess at. `dossier schema` reports the difference instead of
// discovering it later as a mystery exit 1 in the middle of somebody's script.
//
// This is also the honest substitute for a unit test asserting the map is complete: a test
// in this repository can only compare the map against a list written next to it, which
// proves the two lists match each other and nothing about the server.
func Unmapped(schemaCodes []string) []string {
	var missing []string
	for _, code := range schemaCodes {
		if _, known := byCode[code]; !known {
			missing = append(missing, code)
		}
	}
	slices.Sort(missing)
	return missing
}
