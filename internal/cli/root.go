package cli

import (
	"time"

	"github.com/spf13/cobra"
)

// NewRoot builds the command tree.
//
// Errors are silenced at the cobra level and handled by the caller, because cobra's
// default is to print the error *and* the full usage text on any failure. For a CLI whose
// errors are the API's own carefully written message and hint, burying those under sixty
// lines of flag documentation would throw away the copy that matters. Usage is printed
// for usage errors only, which is the one case it helps.
func NewRoot(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "dossier",
		Short: "A command-line client for a Dossier vault's API",
		Long: "dossier drives the machine-facing API of a Dossier vault: read your identity\n" +
			"fields, mint a dossier with an expiry, watch what you have shared, and open a\n" +
			"share as its recipient.\n\n" +
			"It speaks only to /api/v1. It holds a token you minted in Settings; it cannot\n" +
			"mint, rotate or revoke one, and it never opens a browser session.",

		SilenceErrors: true,
		SilenceUsage:  true,

		// Set explicitly so cobra's legacyArgs validator is bypassed. With a nil Args and
		// subcommands present, cobra rejects an unrecognised first argument itself, and
		// its error is not one of ours — so `dossier frobnicate` would exit 1
		// ("unexpected: a CLI bug") when it is plainly a usage error. Taking the
		// arguments here lets the refusal below own the exit code.
		Args: cobra.ArbitraryArgs,

		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return Usagef("Unknown command %q. Run `dossier --help` for the list.\n", args[0])
			}
			return cmd.Help()
		},
	}

	// A malformed flag is also a refusal before anything was sent, so it is exit 2 rather
	// than the generic failure cobra's own error would map to.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return Usagef("%s\n\nRun `%s --help` for the accepted flags.\n", err, cmd.CommandPath())
	})

	flags := root.PersistentFlags()
	// See the note in login.go: a backticked word here becomes the argument placeholder,
	// so this rendered as "--profile default".
	flags.StringVar(&app.ProfileFlag, "profile", "", "Which stored credential to use (defaults to the profile named default, or $DOSSIER_PROFILE)")
	flags.StringVar(&app.HostFlag, "host", "", "Override the profile's host, e.g. http://localhost:3000")
	flags.BoolVar(&app.JSON, "json", false, "Print the API response body verbatim to stdout; errors as the API envelope to stderr")
	flags.BoolVar(&app.NoColor, "no-color", false, "Force colour off (also: NO_COLOR set, TERM=dumb, or stdout not a terminal)")
	flags.BoolVar(&app.NoWait, "no-wait", false, "Do not sleep on Retry-After; fail immediately instead")
	flags.DurationVar(&app.Timeout, "timeout", 0, "Per-request HTTP timeout (default 30s)")
	flags.BoolVarP(&app.Quiet, "quiet", "q", false, "Suppress the confirmation prose; data blocks still print")

	root.AddCommand(
		newVersionCmd(app),
		newSchemaCmd(app),
		newLoginCmd(app),
		newLogoutCmd(app),
		newWhoamiCmd(app),
		newProfilesCmd(app),
		newFieldsCmd(app),
		newDocumentsCmd(app),
		newSharesCmd(app),
		newOpenCmd(app),
	)

	// After AddCommand, because it looks the subcommands up by name.
	registerCompletions(app, root)

	return root
}

// timeout resolves the per-request budget: the flag if set, otherwise the default.
func (a *App) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return 0
}
