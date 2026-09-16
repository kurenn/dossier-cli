package cli

import (
	"github.com/kurenn/dossier-cli/internal/store"
	"github.com/spf13/cobra"
)

func newLogoutCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget a stored token (this does not revoke it)",
		Long: "Deletes the profile's token from the local credentials file.\n\n" +
			"This does not revoke the token. dossier cannot revoke anything — the API has\n" +
			"no token-management surface — so a token forgotten here still works for\n" +
			"anyone who holds a copy until you revoke it from Settings.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runLogout()
		},
	}
}

func (a *App) runLogout() error {
	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	creds, err := a.Credentials()
	if err != nil {
		return err
	}

	if !creds.Delete(resolution.ProfileName) {
		// Not an error. The end state the holder asked for is "this machine has no token
		// for that profile", and that is already true — failing would be pedantry, and
		// would make `logout` unsafe to put in a teardown script.
		a.Notef("No stored token for profile %q; nothing to forget.\n", resolution.ProfileName)
		return nil
	}

	if err := creds.Save(a.Paths); err != nil {
		return Unexpectedf("%s\n", err)
	}
	// The cached discovery document goes too. It is not secret, but leaving a cache keyed
	// to a host the holder just disconnected from is a stale artefact with no owner.
	if err := store.ForgetSchema(a.Paths, resolution.Host); err != nil {
		a.Notef("Could not clear the schema cache: %s\n", err)
	}

	// The whole point of the command's copy: a holder who ran `logout` because a token
	// leaked has not fixed anything yet, and this is the only moment they are certain to
	// be reading.
	a.Notef("Forgot the token for profile %q.\n\n", resolution.ProfileName)
	a.Notef("This did not revoke it. If that token has leaked, revoke it now:\n")
	a.Notef("  %s/settings -> API tokens\n", resolution.Host)

	if resolution.TokenFromEnv {
		a.Notef("\nDOSSIER_TOKEN is still set in this shell, so commands will keep using it.\n")
	}
	return nil
}
