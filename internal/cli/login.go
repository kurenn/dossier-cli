package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
	"github.com/spf13/cobra"
)

// tokenPrefix and tokenTailLength mirror ApiToken's own shape.
//
// Checked locally for one narrow reason: to catch a paste that lost a character before it
// spends one of the ten attempts per minute the server's per-IP failed-auth limiter
// allows. It is not a security check — a well-formed string proves nothing — and the
// server remains the only authority on whether a token is real.
const (
	tokenPrefix     = "dsk_"
	tokenTailLength = 32

	// The Bitcoin base58 alphabet Ruby's SecureRandom.base58 uses: no 0, O, I or l,
	// precisely so a transcribed token cannot be ambiguous.
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

// loginPreface is the command's first line of output, and it is load-bearing copy.
//
// §5.1: the API cannot mint, list or revoke a token — a token comes from Settings, inside
// a browser session, behind a fresh passkey assertion. So `login` cannot mean
// "authenticate me", and the one thing that stops it being read that way is saying so
// before asking for anything.
const loginPreface = "dossier login stores a token you already minted. It cannot mint one.\n" +
	"Mint it from Settings -> API tokens, then paste it here.\n\n"

func newLoginCmd(app *App) *cobra.Command {
	var (
		tokenStdin bool
		scopes     string
		expiresOn  string
		name       string
		tokenFlag  string
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an API token you minted in Settings",
		Long: "Verifies a token works and stores it for a profile.\n\n" +
			"dossier cannot mint a token: the API has no token-management surface at all.\n" +
			"Mint one from Settings -> API tokens in your vault — it is shown once — then\n" +
			"paste it here.\n\n" +
			"The token is read from a hidden prompt, or from standard input with\n" +
			"--token-stdin. It is never accepted as a command-line argument.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Both refusals exist because a token on the command line is recorded in the
			// shell's history file and visible in `ps` to every other user on the
			// machine. Accepting it "just this once" would make the hidden prompt
			// decorative.
			if tokenFlag != "" {
				return Usagef(
					"dossier will not take a token from --token.\n\n" +
						"A token on the command line is written to your shell history and is visible\n" +
						"in `ps` to every other user on this machine. Run `dossier login` and paste it\n" +
						"at the prompt, or pipe it in with --token-stdin.\n",
				)
			}
			if len(args) > 0 {
				return Usagef(
					"dossier will not take a token as an argument.\n\n" +
						"A token on the command line is written to your shell history and is visible\n" +
						"in `ps` to every other user on this machine. Run `dossier login` with no\n" +
						"arguments and paste it at the prompt, or pipe it in with --token-stdin.\n",
				)
			}
			return app.runLogin(cmd.Context(), loginOptions{
				tokenStdin: tokenStdin,
				scopes:     scopes,
				expiresOn:  expiresOn,
				name:       name,
			})
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&tokenStdin, "token-stdin", false, "Read the token from standard input instead of prompting")
	flags.StringVar(&scopes, "scopes", "", "The scopes you gave this token, comma or space separated (your statement; the API does not report them)")
	flags.StringVar(&expiresOn, "expires-on", "", "The expiry date shown in Settings, YYYY-MM-DD (your statement; the API does not report it)")
	// No backticks: cobra reads a backticked word in a flag's usage string as the argument
	// placeholder, so this rendered as "--name dossier profiles list" instead of
	// "--name string". Prose elsewhere in the CLI uses them freely; a flag usage string is
	// the one place they mean something else.
	flags.StringVar(&name, "name", "", "A label for this profile, shown by: dossier profiles list")

	// Defined only so it can be refused with a reason. Without it, cobra answers "unknown
	// flag" — technically correct and completely unhelpful about why.
	flags.StringVar(&tokenFlag, "token", "", "")
	_ = flags.MarkHidden("token")

	return cmd
}

type loginOptions struct {
	tokenStdin bool
	scopes     string
	expiresOn  string
	name       string
}

func (a *App) runLogin(ctx context.Context, opts loginOptions) error {
	resolution, err := a.Resolve()
	if err != nil {
		return err
	}

	if !opts.tokenStdin && !a.interactive() {
		return Usagef(
			"Standard input is not a terminal, so there is nobody to prompt.\n\n" +
				"Pipe the token in instead:\n\n" +
				"    printf %%s \"$TOKEN\" | dossier login --token-stdin\n",
		)
	}

	if !opts.tokenStdin {
		a.Notef("%s", loginPreface)
	}

	token, err := a.readSecret(fmt.Sprintf("Token for profile %q: ", resolution.ProfileName))
	if err != nil {
		return err
	}
	if err := validateTokenShape(token); err != nil {
		return err
	}

	client, err := a.Client(store.Resolution{Host: resolution.Host, Token: token})
	if err != nil {
		return err
	}

	// The schema first, for two reasons beyond warming the cache: it proves the host
	// actually is a Dossier before a credential is sent to it, and it is the source for
	// the scope list the checklist below is built from. It needs no token, so a wrong
	// --host fails here having sent nothing secret anywhere.
	schema, _, _, err := a.schema(ctx, client, true)
	if err != nil {
		return err
	}

	verifiedAt, err := a.probeToken(ctx, client)
	if err != nil {
		return err
	}

	statedScopes, err := a.resolveStatedScopes(opts, schema)
	if err != nil {
		return err
	}
	statedExpiry, err := a.resolveStatedExpiry(opts)
	if err != nil {
		return err
	}

	creds, err := a.Credentials()
	if err != nil {
		return err
	}
	creds.Put(resolution.ProfileName, store.Profile{
		Host:       client.Host(),
		Token:      token,
		Prefix:     strings.TrimPrefix(token, tokenPrefix)[:8],
		Name:       opts.name,
		Scopes:     statedScopes,
		ExpiresOn:  statedExpiry,
		VerifiedAt: verifiedAt.UTC().Format(time.RFC3339),
	})
	if err := creds.Save(a.Paths); err != nil {
		return Unexpectedf("%s\n", err)
	}

	// On a machine that had no config at all, this profile becomes the default: a holder
	// who logged in once should not then have to pass --profile to use the only thing
	// they stored.
	//
	// Keyed on the file's absence rather than on DefaultProfile being empty, because it
	// never is — LoadConfig fills in a fallback. Once a config exists the default is the
	// holder's choice and `login` must not move it; `profiles use` is how it changes.
	config, err := a.Config()
	if err != nil {
		return err
	}
	if !config.Existed() {
		config.DefaultProfile = resolution.ProfileName
		if err := config.Save(a.Paths); err != nil {
			return Unexpectedf("%s\n", err)
		}
	}

	a.Notef("Stored. The token is in %s, mode 0600.\n\n", a.Paths.ConfigDir+"/credentials.toml")

	block := render.NewBlock(9).
		Add("PROFILE", resolution.ProfileName).
		Add("HOST", client.Host()).
		Add("TOKEN", tokenPrefix+strings.TrimPrefix(token, tokenPrefix)[:8]+"…").
		Add("VERIFIED", verifiedAt.UTC().Format(time.RFC3339))
	return block.Render(a.Stdout)
}

// probeToken spends one read request to prove the token authenticates.
//
// The status mapping is the subtle part, and it follows from BaseController running
// authenticate_api_token! as a before_action ahead of every require_scope!:
//
//   - 200 — authenticates, and has fields:read.
//   - 403 — authenticates, and does not have fields:read. Still a live token, which is why
//     this is a success here. A read-only token minted for scripts would land here.
//   - 401 — does not authenticate. Expired, revoked or never valid; the API makes those
//     three indistinguishable on purpose and so does this.
//   - 429 — proves *neither*. Both the per-token read limiter and the per-IP failed-auth
//     counter surface as rate_limited, so the probe did not run. Nothing is stored, and
//     the exit is TryLater rather than a guess in either direction.
func (a *App) probeToken(ctx context.Context, client *api.Client) (time.Time, error) {
	_, err := client.Do(ctx, api.Request{
		Method:  http.MethodGet,
		Path:    api.Prefix + "/fields",
		Query:   url.Values{"limit": []string{"1"}},
		Timeout: a.timeout(),
	})
	if err == nil {
		return a.Now(), nil
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return time.Time{}, Classify(err)
	}

	switch apiErr.Code {
	case "insufficient_scope":
		a.Notef("This token authenticates but has no fields:read scope. Stored anyway.\n")
		return a.Now(), nil
	case "rate_limited":
		return time.Time{}, &Error{
			Code: exitcode.TryLater,
			Message: "Could not verify the token: the server is rate limiting this client.\n\n" +
				"A 429 here means the check did not run — it does not mean the token is bad.\n" +
				"Nothing has been stored. Try again in a minute.\n",
			Err: apiErr,
		}
	default:
		return time.Time{}, a.classify(apiErr, store.Resolution{})
	}
}

// resolveStatedScopes gets the scopes the holder says the token has.
//
// "Says" is the operative word and it is carried all the way to `whoami`: the API has no
// introspection endpoint, so nothing here is ever confirmed. Validated against the
// schema's scope list only to catch a typo — an unknown scope is certainly wrong, while a
// known one is merely plausible.
func (a *App) resolveStatedScopes(opts loginOptions, schema *api.Schema) ([]string, error) {
	if opts.scopes != "" {
		stated := splitList(opts.scopes)
		if unknown := unknownScopes(stated, schema.Scopes); len(unknown) > 0 {
			return nil, Usagef(
				"This API does not have the scope(s): %s\n\nIt publishes: %s\n",
				strings.Join(unknown, " "), strings.Join(schema.Scopes, " "),
			)
		}
		return stated, nil
	}

	if !a.interactive() || len(schema.Scopes) == 0 {
		return nil, nil
	}

	a.Notef("\nWhich scopes did you give this token? Dossier cannot tell us, so this is\n")
	a.Notef("recorded as your own note. Enter numbers, or press enter to skip.\n\n")
	for index, scope := range schema.Scopes {
		a.Notef("  %d) %s\n", index+1, scope)
	}

	answer, err := a.readLine("\nScopes: ")
	if err != nil {
		return nil, err
	}
	if answer == "" {
		return nil, nil
	}

	var stated []string
	for _, choice := range splitList(answer) {
		index := 0
		if _, err := fmt.Sscanf(choice, "%d", &index); err != nil || index < 1 || index > len(schema.Scopes) {
			return nil, Usagef("%q is not one of the numbers listed.\n", choice)
		}
		stated = append(stated, schema.Scopes[index-1])
	}
	return stated, nil
}

func (a *App) resolveStatedExpiry(opts loginOptions) (string, error) {
	if opts.expiresOn != "" {
		if _, err := time.Parse(time.DateOnly, opts.expiresOn); err != nil {
			return "", Usagef("--expires-on must be YYYY-MM-DD; got %q.\n", opts.expiresOn)
		}
		return opts.expiresOn, nil
	}
	if !a.interactive() {
		return "", nil
	}

	answer, err := a.readLine("Expiry date from Settings (YYYY-MM-DD, or enter to skip): ")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return "", nil
	}
	if _, err := time.Parse(time.DateOnly, answer); err != nil {
		return "", Usagef("That is not a YYYY-MM-DD date: %q.\n", answer)
	}
	return answer, nil
}

// validateTokenShape checks the paste locally. See tokenPrefix for why this exists and
// what it deliberately does not claim.
func validateTokenShape(token string) error {
	if token == "" {
		return Usagef("No token was entered.\n")
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		return Usagef(
			"That does not look like a Dossier API token: they begin with %q.\n\n"+
				"Copy it from Settings -> API tokens, where it is shown once at mint.\n",
			tokenPrefix,
		)
	}

	tail := strings.TrimPrefix(token, tokenPrefix)
	if len(tail) != tokenTailLength {
		return Usagef(
			"That token is %d characters after %q; a Dossier token has %d.\n\n"+
				"This usually means the paste was truncated. Checked locally, so no attempt\n"+
				"has been spent against the server.\n",
			len(tail), tokenPrefix, tokenTailLength,
		)
	}
	for _, char := range tail {
		if !strings.ContainsRune(base58Alphabet, char) {
			return Usagef(
				"That token contains %q, which a Dossier token never does.\n\n"+
					"Tokens avoid 0, O, I and l so they cannot be transcribed ambiguously.\n"+
					"This usually means a character was mangled in copying.\n",
				char,
			)
		}
	}
	return nil
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func unknownScopes(stated, known []string) []string {
	var unknown []string
	for _, scope := range stated {
		found := false
		for _, candidate := range known {
			if scope == candidate {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, scope)
		}
	}
	return unknown
}
