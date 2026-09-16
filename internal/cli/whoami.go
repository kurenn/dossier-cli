package cli

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
	"github.com/spf13/cobra"
)

func newWhoamiCmd(app *App) *cobra.Command {
	var probe bool

	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the active profile, host and token prefix",
		Long: "Reports which credential is in use.\n\n" +
			"Dossier has no token introspection endpoint, so this separates what dossier\n" +
			"knows from what you told it at login, and labels which is which. --probe\n" +
			"spends one request per scope to find out which ones the token really has.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runWhoami(cmd.Context(), probe)
		},
	}
	cmd.Flags().BoolVar(&probe, "probe", false, "Test each scope with one side-effect-free request")
	return cmd
}

func (a *App) runWhoami(ctx context.Context, probe bool) error {
	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	if resolution.ProfileMissing && resolution.Token == "" {
		return Usagef(
			"No profile named %q.\n\nRun `dossier login` to store a token, or `dossier profiles list`\nto see what is here.\n",
			resolution.ProfileName,
		)
	}
	if err := a.requireToken(resolution); err != nil {
		return err
	}

	// Facts first: things the CLI actually knows, because it holds the file or because a
	// probe proved them at a moment in time.
	facts := render.NewBlock(12).
		Add("PROFILE", resolution.ProfileName).
		Add("HOST", resolution.Host).
		Add("TOKEN", maskToken(resolution.Token))

	if resolution.TokenFromEnv {
		facts.Add("SOURCE", "DOSSIER_TOKEN (environment)")
	} else {
		facts.Add("SOURCE", a.Paths.ConfigDir+"/credentials.toml")
	}
	if name := resolution.Profile.Name; name != "" && !resolution.TokenFromEnv {
		facts.Add("NAME", name)
	}
	// VERIFIED is only a fact about the *stored* token: it records when a probe proved
	// that one authenticated. Under DOSSIER_TOKEN the token in use is a different string
	// that this machine may never have verified at all, so showing the stored timestamp
	// beside it would attribute a check to a credential that never had one.
	if verified := resolution.Profile.VerifiedAt; verified != "" && !resolution.TokenFromEnv {
		// The parenthetical is the honest part. A successful probe proves the token
		// authenticated at that instant and nothing about now, and it says nothing about
		// scope — login treats a 403 as a pass precisely because it means the token is
		// live.
		facts.Add("VERIFIED", verified+"  (authenticated; scope not checked)")
	}
	if err := facts.Render(a.Stdout); err != nil {
		return err
	}

	// Then statements, under a heading that says the API will not confirm them. The split
	// is the whole design of this command (§5.4): presenting a holder's note about scopes
	// in the same block as the host would let it be read as something the server said.
	//
	// Suppressed entirely when the token came from the environment, because the stored
	// statements describe the *profile's* token and this is a different one. Printing them
	// here would be worse than printing nothing: they would read as a description of the
	// credential actually in use, and a --probe disagreeing with them would look like the
	// probe was broken rather than like the two were never related.
	stated := resolution.Profile.Scopes
	if resolution.TokenFromEnv && (len(stated) > 0 || resolution.Profile.ExpiresOn != "") {
		a.Println()
		a.Println("The stored profile's scopes and expiry are not shown: this token came from")
		a.Println("DOSSIER_TOKEN, so what you stated at login describes a different token.")
	} else if len(stated) > 0 || resolution.Profile.ExpiresOn != "" {
		a.Println()
		a.Println("Stated at login — Dossier does not report these over the API:")
		statements := render.NewBlock(12)
		if len(stated) > 0 {
			statements.Add("SCOPES", strings.Join(stated, " "))
		}
		if expires := resolution.Profile.ExpiresOn; expires != "" {
			statements.Add("EXPIRES ON", expires+expiryAside(expires, a.Now()))
		}
		if err := statements.Render(a.Stdout); err != nil {
			return err
		}
	}

	if !probe {
		return nil
	}
	return a.runProbe(ctx, resolution)
}

// scopeProbe is one side-effect-free request that distinguishes "has this scope" from
// "does not".
//
// Every probe is chosen so a token that *does* have the scope still changes nothing. That
// is what makes --probe safe to run against a live vault, and it relies on the API
// checking scope before it validates a body or claims an idempotency key — documented
// ordering, but ordering no endpoint was designed to expose, which is why this is behind
// a flag with the caveat printed.
type scopeProbe struct {
	scope   string
	method  string
	path    string
	query   url.Values
	body    []byte
	hasCode string
	note    string
}

func scopeProbes() []scopeProbe {
	return []scopeProbe{
		{scope: "fields:read", method: http.MethodGet, path: api.Prefix + "/fields",
			query: url.Values{"limit": []string{"1"}}, note: "reads one field"},
		{scope: "shares:read", method: http.MethodGet, path: api.Prefix + "/shares",
			query: url.Values{"limit": []string{"1"}}, note: "reads one share"},
		{scope: "fields:write", method: http.MethodPost, path: api.Prefix + "/fields",
			body: []byte(`{}`), hasCode: "validation_failed", note: "an empty body; nothing is created"},
		{scope: "documents:write", method: http.MethodPost, path: api.Prefix + "/fields/0/documents",
			hasCode: "not_found", note: "field 0, which cannot exist; nothing is created"},
		{scope: "shares:mint", method: http.MethodPost, path: api.Prefix + "/shares",
			body: []byte(`{}`), hasCode: "idempotency_key_required",
			note: "no Idempotency-Key, refused before any claim is made"},
	}
}

func (a *App) runProbe(ctx context.Context, resolution store.Resolution) error {
	client, err := a.Client(resolution)
	if err != nil {
		return err
	}

	a.Notef("\nProbing each scope with one request. Nothing below creates or changes\n")
	a.Notef("anything, but each probe does spend one unit of that endpoint's rate limit.\n")

	a.Println()
	results := render.NewBlock(16)

	for _, probe := range scopeProbes() {
		verdict, err := a.probeScope(ctx, client, probe)
		if err != nil {
			// A token that does not authenticate is not a scope result, and reporting it
			// as one five times over — then exiting 0 — would tell a script the check
			// succeeded. The first 401 ends the command with the shared §5.5 copy and
			// exit 3, which is what a caller can actually act on.
			return a.classify(err, resolution)
		}
		results.AddRaw(probe.scope, verdict)
	}
	return results.Render(a.Stdout)
}

// probeScope returns "has", "lacks", or an honest description of why neither is known.
//
// A non-nil error means the probe could not answer the question at all — the token is
// dead, or the transport failed — and is returned rather than rendered, so the caller can
// stop instead of filling a column with the same non-answer.
func (a *App) probeScope(ctx context.Context, client *api.Client, probe scopeProbe) (string, error) {
	_, err := client.Do(ctx, api.Request{
		Method:  probe.method,
		Path:    probe.path,
		Query:   probe.query,
		Body:    probe.body,
		Timeout: a.timeout(),
	})

	if err == nil {
		// A 2xx is only the expected answer for the two read probes; the write probes are
		// designed to fail in a specific way, so a success there means the API changed
		// and this probe is no longer side-effect-free. Saying so beats reporting "has".
		if probe.hasCode == "" {
			return "has", nil
		}
		return "unclear — the probe succeeded, which it was not supposed to do", nil
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return "", err
	}

	switch apiErr.Code {
	case "insufficient_scope":
		return "lacks", nil
	case probe.hasCode:
		return "has", nil
	case "unauthenticated":
		return "", apiErr
	case "rate_limited":
		return "unknown — rate limited, so the probe did not run", nil
	default:
		return "unclear — " + apiErr.Code, nil
	}
}

// maskToken shows the prefix the Settings table shows and nothing more. Enough to match a
// row by eye; useless to anyone reading over a shoulder or scrolling a terminal log.
func maskToken(token string) string {
	tail := strings.TrimPrefix(token, tokenPrefix)
	if len(tail) <= 8 {
		return tokenPrefix + tail
	}
	return tokenPrefix + tail[:8] + "…"
}

// expiryAside adds a note when the holder's own stated expiry has passed.
//
// Phrased as their statement, not as a finding: the CLI has no way to know a token's real
// expiry, and the stored date could simply be wrong. The token is never deleted on this
// basis (§5.5) — the holder may be offline, or on the wrong host, and destroying a
// credential over an unverifiable local note would be the worst possible trade.
func expiryAside(expiresOn string, now time.Time) string {
	parsed, err := time.Parse(time.DateOnly, expiresOn)
	if err != nil {
		return ""
	}
	if parsed.After(now) {
		return ""
	}
	return "  (you said this date; it has passed)"
}
