package cli

import (
	"context"
	"strconv"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/spf13/cobra"
)

func newFieldsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fields",
		Short: "The identity fields in your vault",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newFieldsListCmd(app), newFieldsCreateCmd(app))
	return cmd
}

func newFieldsListCmd(app *App) *cobra.Command {
	var opts listOptions

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the fields in your vault",
		Long: "Lists field metadata: id, label, category, country, sensitivity, status and a\n" +
			"count of attached documents.\n\n" +
			"Never a value. The API does not return one from this endpoint and this command\n" +
			"has no column for one — a field's value reaches a screen only through the\n" +
			"dossier you released it in.\n\n" +
			"--status takes any status the API publishes in its schema, which is all four a\n" +
			"vault can hold, not just the two you usually want: an `empty` field is one you\n" +
			"created and have not filled, and a `withdrawn` one is still on file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runFieldsList(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.filter, "status", "", "Only fields with this status (see: dossier schema)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Rows per page; the API's default and maximum are in: dossier schema")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Follow pagination to the end")
	return cmd
}

func (a *App) runFieldsList(ctx context.Context, opts listOptions) error {
	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	if err := a.requireToken(resolution); err != nil {
		return err
	}
	client, err := a.Client(resolution)
	if err != nil {
		return err
	}

	limit, err := resolveLimit(opts.limit)
	if err != nil {
		return err
	}

	// The schema is read before the request so the refusal for a bad --status costs
	// nothing and can name the real vocabulary. It is cached for 24h, so this is a file
	// read on all but the first run of a day.
	schema, _, _, err := a.schema(ctx, client, false)
	if err != nil {
		return err
	}
	if err := validateVocabulary("status", opts.filter, schema.FieldStatuses); err != nil {
		return err
	}

	var collected []api.Field
	var pages int
	err = pager(ctx, opts.all, func(ctx context.Context, after *int) (*int, error) {
		page, err := client.ListFields(ctx, api.FieldQuery{
			Status: opts.filter,
			Limit:  limit,
			After:  after,
		}, a.timeout())
		if err != nil {
			return nil, Classify(err)
		}
		pages++

		// --json streams: one document per page, verbatim, because §7.4 requires each to
		// stay exactly something the server said. Human output accumulates instead, so
		// that --all is one table with one set of column widths. Printing a page at a
		// time would recompute widths per page and the columns would visibly jump
		// mid-table, which is worse than the cost of holding a vault's field list in
		// memory — this is a personal document vault, not a log.
		if a.JSON {
			a.emitRawPage(page.Raw)
		} else {
			collected = append(collected, page.Fields...)
		}
		return page.NextAfter, nil
	})
	if err != nil {
		return err
	}

	if a.JSON {
		return nil
	}
	if len(collected) == 0 {
		a.emptyNote("fields", statusFilterNote(opts.filter))
		return nil
	}
	a.renderFieldsTable(collected)
	a.countNote(len(collected), pages, "field")
	return nil
}

func statusFilterNote(status string) string {
	if status == "" {
		return ""
	}
	return "--status " + status
}

// renderFieldsTable writes the collected rows as one table.
//
// Columns are the API row's own keys, in the API's order (§7.3). The one reshaping is the
// nested `documents` array, which has no column shape and becomes a count — its filenames,
// content types and byte sizes are reachable through --json.
//
// `has_document` is kept even though DOCS makes it derivable. The rule is that no scalar
// key is dropped, and honouring it costs a narrow column; dropping it would mean this
// table and the API's own response disagreed about what a field row is, which is the thing
// a holder reading `docs/api.md` beside the CLI must never hit.
func (a *App) renderFieldsTable(fields []api.Field) {
	table := render.NewTable("ID", "LABEL", "CATEGORY", "COUNTRY", "SENSITIVITY", "STATUS", "HAS_DOCUMENT", "DOCS")
	for _, field := range fields {
		country := ""
		if field.Country != nil {
			country = *field.Country
		}
		table.Add(
			strconv.Itoa(field.ID),
			field.Label,
			field.Category,
			country,
			strconv.Itoa(field.Sensitivity),
			field.Status,
			strconv.FormatBool(field.HasDocument),
			strconv.Itoa(len(field.Documents)),
		)
	}
	_ = table.Render(a.Stdout)
}
