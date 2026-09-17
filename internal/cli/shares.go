package cli

import (
	"context"
	"strconv"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/spf13/cobra"
)

func newSharesCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shares",
		Short: "The dossiers you have released",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newSharesListCmd(app), newSharesShowCmd(app))
	return cmd
}

func newSharesListCmd(app *App) *cobra.Command {
	var opts listOptions

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the dossiers you have released",
		Long: "Lists shares newest first: id, token, title, recipient, state, deadline, opens\n" +
			"and field count.\n\n" +
			"The deadline column shows how long a live dossier has left, and for one that has\n" +
			"closed it shows the closing fact instead — never a countdown on a link that no\n" +
			"longer works.\n\n" +
			"--state takes any state the API publishes in its schema. Note that the API\n" +
			"derives state at read time and filters after fetching a page, so a filtered page\n" +
			"can come back shorter than --limit while matching rows sit further back; --all\n" +
			"follows the cursor to the end regardless.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runSharesList(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.filter, "state", "", "Only shares in this state (see: dossier schema)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Rows per page; the API's default and maximum are in: dossier schema")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Follow pagination to the end")
	return cmd
}

func (a *App) runSharesList(ctx context.Context, opts listOptions) error {
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

	schema, _, _, err := a.schema(ctx, client, false)
	if err != nil {
		return err
	}
	if err := validateVocabulary("state", opts.filter, schema.ShareStates); err != nil {
		return err
	}

	var collected []api.Share
	var pages int
	err = pager(ctx, opts.all, func(ctx context.Context, after *int) (*int, error) {
		page, err := client.ListShares(ctx, api.ShareQuery{
			State: opts.filter,
			Limit: limit,
			After: after,
		}, a.timeout())
		if err != nil {
			return nil, Classify(err)
		}
		pages++

		if a.JSON {
			a.emitRawPage(page.Raw)
		} else {
			collected = append(collected, page.Shares...)
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
		a.emptyNote("shares", stateFilterNote(opts.filter))
		return nil
	}
	a.renderSharesTable(collected)
	a.countNote(len(collected), pages, "share")
	return nil
}

func stateFilterNote(state string) string {
	if state == "" {
		return ""
	}
	return "--state " + state
}

// renderSharesTable writes the collected rows as one table.
//
// The columns are the plan's §7.3 example, which is where this diverges from the letter of
// the same section's "no scalar key is dropped, and nothing is renamed". Three things
// happen to the API's keys here, each for a reason the example already assumes:
//
//   - `recipient` is an object, and an object has no column shape any more than an array
//     does, so it renders as its label (falling back to the email).
//   - `expires_at` and `revoked_at` become one DEADLINE column, because §7.1 rule 3 says
//     the countdown appears there and only there, and because neither key answers the
//     deadline question alone.
//   - `opens_count` and `field_count` are headed OPENS and FIELDS, as the example heads
//     them. The `_count` suffix is the API naming a scalar; a column of integers under a
//     count's name does not need it repeated.
//
// And `batch_id` is dropped, the one scalar that is. It is a 36-character UUID that would
// take more width than every other column combined and is a grouping key for siblings of
// one mint — which `shares show` gives in a block that has no width pressure, and --json
// gives always. Dropping it from a table read by eye is the only one of these four
// decisions that loses something, and it loses it in exactly one of three places.
func (a *App) renderSharesTable(shares []api.Share) {
	colors := a.Colors()
	now := a.Now()

	table := render.NewTable("ID", "TOKEN", "TITLE", "RECIPIENT", "STATE", "DEADLINE", "OPENS", "FIELDS")
	for _, share := range shares {
		state := render.State(share.State)
		deadline := render.Deadline(state, share.ExpiresAt, share.RevokedAt, now)

		table.Add(
			strconv.Itoa(share.ID),
			share.Token,
			derefOr(share.Title, ""),
			share.Recipient.Display(),
			colors.StateWord(state),
			colors.Countdown(state, deadline),
			strconv.Itoa(share.OpensCount),
			strconv.Itoa(share.FieldCount),
		)
	}
	_ = table.Render(a.Stdout)
}

func newSharesShowCmd(app *App) *cobra.Command {
	var noAudit bool

	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show one dossier and its audit trail",
		Long: "Shows one share as a block, then its audit trail.\n\n" +
			"Every audit row carries a name and a sentence saying what it means, both of them\n" +
			"the server's own words. The trail is what the API knows happened and when; it\n" +
			"records no IP address and no device fingerprint, so a run of opens tells you the\n" +
			"link was opened that many times, not by whom.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runSharesShow(cmd.Context(), args[0], !noAudit)
		},
	}
	// The flag is --no-audit rather than --audit because the plan has the trail on by
	// default in a TTY, and a flag whose default flips with the terminal is a flag nobody
	// can predict. It is on everywhere instead: the trail is most of the point of this
	// command, and a script that does not want it asks for --json and reads the share key.
	cmd.Flags().BoolVar(&noAudit, "no-audit", false, "Show only the share, without its audit trail")
	return cmd
}

func (a *App) runSharesShow(ctx context.Context, idArg string, withAudit bool) error {
	id, err := strconv.Atoi(idArg)
	if err != nil {
		return Usagef("%q is not a share id. Ids are the integers `dossier shares list` prints.\n", idArg)
	}

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

	detail, err := client.ShowShare(ctx, id, a.timeout())
	if err != nil {
		return Classify(err)
	}

	if a.JSON {
		a.emitRawPage(detail.Raw)
		return nil
	}

	a.renderShareBlock(detail.Share)
	if withAudit {
		a.Println()
		a.renderAuditTrail(detail.AuditEvents)
	}
	return nil
}

// renderShareBlock writes one share as a key–value block.
//
// Every key the API sent, including the `batch_id` the table has no room for, and
// including both halves of the recipient rather than the table's single cell. A block has
// no width pressure, so the rule §7.3 states — nothing dropped — is kept literally here.
//
// DEADLINE is derived, as in the table. EXPIRES and REVOKED are also shown raw beneath it,
// because this is the surface where a holder is looking at one share closely: the derived
// figure answers "how long", the timestamps answer "exactly when", and rule 3 wants both
// rendered as the API sent them.
func (a *App) renderShareBlock(share api.Share) {
	colors := a.Colors()
	state := render.State(share.State)
	deadline := render.Deadline(state, share.ExpiresAt, share.RevokedAt, a.Now())

	block := render.NewBlock(len("RECIPIENT"))
	block.Add("ID", strconv.Itoa(share.ID))
	block.Add("TOKEN", share.Token)
	block.Add("TITLE", derefOr(share.Title, ""))
	block.Add("RECIPIENT", share.Recipient.Display())
	block.Add("EMAIL", share.Recipient.Email)
	block.AddRaw("STATE", colors.StateWord(state))
	block.AddRaw("DEADLINE", colors.Countdown(state, orEmptyCell(deadline)))
	block.Add("EXPIRES", formatTimestamp(share.ExpiresAt))
	block.Add("REVOKED", formatTimestamp(share.RevokedAt))
	block.Add("OPENS", strconv.Itoa(share.OpensCount))
	block.Add("FIELDS", strconv.Itoa(share.FieldCount))
	block.Add("BATCH", derefOr(share.BatchID, ""))
	_ = block.Render(a.Stdout)
}

// renderAuditTrail writes the trail as rule 9's name-and-sentence pairs.
//
// Not a table, deliberately. A `meaning` is a full sentence — "The dossier was sealed and
// the link created." — and a sentence in a table cell either wraps or forces the column to
// the width of the longest one, and §7.1 rule 1 forbids prose and data sharing a line
// regardless. So each event is a timestamped heading line of data, then the label and the
// sentence beneath it as prose. Both strings are the server's, verbatim: the CLI writing
// its own gloss for "pin-fail" would be the second source of truth rule 9 exists to stop.
func (a *App) renderAuditTrail(events []api.AuditEvent) {
	if len(events) == 0 {
		// Cannot happen for a share that exists — a mint always writes `issued` — so this
		// is a server saying something this command does not understand rather than an
		// empty result, and it says so instead of printing a bare heading.
		a.Notef("This share has no audit trail, which should not be possible.\n")
		return
	}

	a.Println("AUDIT")
	for _, event := range events {
		a.Printf("  %s  %s\n", event.OccurredAt.UTC().Format(auditTimestamp), event.Label)
		a.Printf("  %s  %s\n", blank(auditTimestamp), event.Meaning)
		if event.Detail != nil && *event.Detail != "" {
			a.Printf("  %s  %s\n", blank(auditTimestamp), *event.Detail)
		}
		a.Println()
	}
}

// auditTimestamp is ISO 8601 UTC to the second. Unlike a deadline, an audit row's
// resolution matters: two events a minute apart are a different story from two events in
// the same second, and the API records real wall-clock (rule 9).
const auditTimestamp = "2006-01-02T15:04:05Z"

func blank(layout string) string {
	return spaces(len(layout))
}

func spaces(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}

func formatTimestamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(auditTimestamp)
}

func orEmptyCell(s string) string {
	if s == "" {
		return render.EmptyCell
	}
	return s
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
