package cli

import (
	"errors"
	"strings"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/store"
)

// unauthenticatedCopy is what the CLI says for every 401 unauthenticated, and it is the
// one place an API message is deliberately *not* passed through verbatim.
//
// The API's own hint is accurate but incomplete for a human: it says to send a token that
// is not expired or revoked, without saying that the server will never tell you which of
// those happened. That ambiguity is a deliberate design property (expired, revoked and
// never-valid are made indistinguishable so the endpoint cannot be used as an oracle),
// and a client that left it unexplained would send people hunting for a distinction that
// does not exist. §5.5 specifies this copy for exactly that reason.
const unauthenticatedCopy = "This token no longer authenticates. Dossier does not say whether it expired,\n" +
	"was revoked, or was never valid — the answer is the same for all three.\n\n" +
	"Mint a new one from Settings -> API tokens and run `dossier login`.\n"

// unauthenticated builds the error for a 401, adding the holder's own stated expiry as a
// separate, clearly-labelled line when it has passed.
//
// The stated date is never presented as the cause. The CLI cannot know a token's real
// expiry, the stored note may simply be wrong, and asserting it as the explanation would
// be inventing a fact to be helpful.
func unauthenticated(profile store.Profile, now time.Time, fromEnv bool) *Error {
	var message strings.Builder
	message.WriteString(unauthenticatedCopy)

	// Skipped when the token came from the environment: the stored date describes the
	// profile's token, and this is a different one.
	if !fromEnv && profile.ExpiresOn != "" {
		if parsed, err := time.Parse(time.DateOnly, profile.ExpiresOn); err == nil && !parsed.After(now) {
			message.WriteString("\nAt login you said this token would expire on " + profile.ExpiresOn + ".\n")
		}
	}

	return &Error{Code: exitcode.Unauthenticated, Message: message.String()}
}

// classify turns a transport or API error into a CLI error, routing 401s through the
// shared copy above so every command reports a dead token identically.
//
// The stored token is deliberately not deleted here (§5.5). A 401 can mean the holder is
// pointed at the wrong host or is offline behind a captive portal, and destroying a
// credential the CLI cannot re-obtain — nothing in this program can mint one — over an
// ambiguous signal would be the worst available trade. `login` over the same profile
// replaces it when the holder is ready.
func (a *App) classify(err error, resolution store.Resolution) error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == "unauthenticated" {
		return unauthenticated(resolution.Profile, a.Now(), resolution.TokenFromEnv)
	}
	return Classify(err)
}
