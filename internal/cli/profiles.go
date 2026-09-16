package cli

import (
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

func newProfilesCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "List stored profiles and choose the default",
		Long: "A profile is one (host, token) pair.\n\n" +
			"Two vaults, or two tokens for one vault — a read-only one for scripts and a\n" +
			"mint-capable one used by hand — are two profiles. The prefix shown here is the\n" +
			"same one the Settings table shows, so a profile can be matched to its row.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newProfilesListCmd(app), newProfilesUseCmd(app))
	return cmd
}

func newProfilesListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List stored profiles",
		Args:    cobra.NoArgs,
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runProfilesList()
		},
	}
}

func (a *App) runProfilesList() error {
	config, err := a.Config()
	if err != nil {
		return err
	}
	creds, err := a.Credentials()
	if err != nil {
		return err
	}

	names := creds.Names()
	if len(names) == 0 {
		a.Notef("No stored profiles. Run `dossier login` to add one.\n")
		return nil
	}

	// The default is marked in the name column rather than given a column of its own: one
	// profile in the list is the default, and a whole column of empty cells to say so
	// would be noise.
	//
	// The marker is built before the widths are measured, not after. Measuring the bare
	// names and then appending " *" is the obvious order and it silently pushes the
	// default's row two characters out of alignment — which a golden file will then
	// record as correct.
	display := make(map[string]string, len(names))
	for _, name := range names {
		display[name] = name
		if name == config.DefaultProfile {
			display[name] = name + " *"
		}
	}

	// Widths from the page, as the list design requires, so the columns fit the data
	// rather than a guess — and so the output stays greppable and sortable without --json.
	nameWidth, hostWidth, prefixWidth, expiresWidth := len("NAME"), len("HOST"), len("PREFIX"), len("EXPIRES")
	for _, name := range names {
		profile := creds.Profiles[name]
		nameWidth = max(nameWidth, utf8.RuneCountInString(display[name]))
		hostWidth = max(hostWidth, utf8.RuneCountInString(orEmpty(profile.Host)))
		prefixWidth = max(prefixWidth, utf8.RuneCountInString(orEmpty(prefixCell(profile.Prefix))))
		expiresWidth = max(expiresWidth, utf8.RuneCountInString(orEmpty(profile.ExpiresOn)))
	}

	a.Printf("%s  %s  %s  %s  %s\n",
		pad("NAME", nameWidth), pad("HOST", hostWidth),
		pad("PREFIX", prefixWidth), pad("EXPIRES", expiresWidth), "LABEL")

	for _, name := range names {
		profile := creds.Profiles[name]
		a.Printf("%s  %s  %s  %s  %s\n",
			pad(display[name], nameWidth),
			pad(orEmpty(profile.Host), hostWidth),
			pad(orEmpty(prefixCell(profile.Prefix)), prefixWidth),
			pad(orEmpty(profile.ExpiresOn), expiresWidth),
			orEmpty(profile.Name),
		)
	}

	a.Notef("\n* is the default. Expiry is what you stated at login, not something\n")
	a.Notef("Dossier reports.\n")
	return nil
}

func newProfilesUseCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Set the default profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runProfilesUse(args[0])
		},
	}
}

func (a *App) runProfilesUse(name string) error {
	creds, err := a.Credentials()
	if err != nil {
		return err
	}
	if _, found := creds.Get(name); !found {
		known := creds.Names()
		if len(known) == 0 {
			return Usagef("No profile named %q, and none are stored. Run `dossier login` first.\n", name)
		}
		return Usagef("No profile named %q. Stored: %s\n", name, strings.Join(known, " "))
	}

	config, err := a.Config()
	if err != nil {
		return err
	}
	config.DefaultProfile = name
	if err := config.Save(a.Paths); err != nil {
		return Unexpectedf("%s\n", err)
	}

	a.Notef("Default profile is now %q.\n", name)
	return nil
}

// pad right-aligns a column on rune count, not byte length — a label or host carrying a
// non-ASCII character would otherwise shift every column after it.
func pad(value string, width int) string {
	gap := width - utf8.RuneCountInString(value)
	if gap < 0 {
		gap = 0
	}
	return value + strings.Repeat(" ", gap)
}

func prefixCell(prefix string) string {
	if prefix == "" {
		return ""
	}
	return tokenPrefix + prefix
}

func orEmpty(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
