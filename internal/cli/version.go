package cli

import (
	"runtime"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/spf13/cobra"
)

func newVersionCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the client version and the API version it targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The API version is printed alongside the client's own because they are
			// independent: a holder filing a bug needs to say which binary spoke which
			// API, and "dossier 0.3.1" alone does not answer that.
			block := render.NewBlock(9).
				Add("DOSSIER", Version).
				Add("API", api.Prefix).
				Add("GO", runtime.Version()).
				Add("PLATFORM", runtime.GOOS+"/"+runtime.GOARCH)

			return block.Render(app.Stdout)
		},
	}
}
