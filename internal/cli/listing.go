package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// listOptions are the flags both list commands share.
type listOptions struct {
	filter string
	limit  int
	all    bool
}

// validateVocabulary checks a flag value against the vocabulary the schema publishes for
// it — `--status`, `--state`, `--category` — and refuses before sending anything.
//
// The vocabulary is never a literal in this binary. That is the CLI's non-negotiable 2 and
// the reason §3 gave for rejecting Ruby: a filter list baked into a release goes stale the
// first time the server grows a state, and the CLI would then refuse a value the API
// accepts. Reading it from the schema means the only way to be wrong is for the schema
// itself to be wrong, which the contract job checks.
//
// Refusing locally rather than letting the API answer 422 is deliberate, and not just to
// save a round trip: `dossier shares list --state lve` should say what the four states are,
// and the schema is what knows them.
func validateVocabulary(flagName, value string, allowed []string) error {
	if value == "" || slices.Contains(allowed, value) {
		return nil
	}

	if len(allowed) == 0 {
		// The schema published no vocabulary for this filter. Sending the value is a
		// better failure than refusing it: the server is the authority and will say
		// what it thinks, whereas refusing here would invent a rule from an absence.
		return nil
	}

	return Usagef(
		"%q is not a --%s this API knows.\n\nIt publishes: %s\n",
		value, flagName, strings.Join(allowed, ", "),
	)
}

// resolveLimit turns --limit into a per-request page size, refusing a nonsense one.
//
// Zero means "unset", and the API's own default (25) applies — the CLI does not restate
// it, because a default duplicated in two places is a default that will disagree with
// itself. The schema publishes the maximum, but the API clamps silently rather than
// failing, so a value above it is passed through: the server's clamp is the honest
// behaviour and second-guessing it here would make --limit 500 an error in the CLI and a
// 100-row page everywhere else.
func resolveLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, Usagef("--limit cannot be negative.\n")
	}
	return limit, nil
}

// pager walks cursor pages until the API stops offering a next cursor.
//
// The subtlety this exists for is in `shares list --state`: the state filter is applied in
// Ruby *after* the page is fetched, so a filtered page can come back shorter than the
// limit — or entirely empty — while matching rows still sit further back. Stopping on a
// short page, which is the obvious way to write this, would silently truncate the answer.
// The only thing that ends the walk is `next_after` being null, which is the server saying
// "that was all of them".
//
// fetch returns the rows it rendered and the next cursor. render is called per page rather
// than after the walk, so --all on a large vault streams rather than buffering, and so
// --json can emit one document per page (§7.4) instead of merging pages into a shape the
// API never produced.
func pager(ctx context.Context, all bool, fetch func(ctx context.Context, after *int) (next *int, err error)) error {
	var after *int
	for {
		next, err := fetch(ctx, after)
		if err != nil {
			return err
		}
		if !all || next == nil {
			return nil
		}
		after = next
	}
}

// emitRawPage writes one page's body verbatim for --json.
//
// One JSON document per page, newline-separated, rather than a merged array: merging would
// invent a shape the API does not have, and would have to decide what to do with each
// page's `next_after`. JSON Lines of pages keeps every document exactly something the
// server said.
func (a *App) emitRawPage(raw []byte) {
	a.Printf("%s", raw)
	if !strings.HasSuffix(string(raw), "\n") {
		a.Printf("\n")
	}
}

// emptyNote is the stderr line for a listing with no rows.
//
// On stderr, not stdout (§7.5): a script doing `dossier shares list --json | jq` must see
// an empty result, not prose. And said at all, rather than printing a bare header, because
// "no rows" and "the command failed quietly" look identical otherwise. An empty list is
// not an error, so the exit code stays 0.
func (a *App) emptyNote(noun string, filter string) {
	if filter == "" {
		a.Notef("No %s.\n", noun)
		return
	}
	a.Notef("No %s matching %s.\n", noun, filter)
}

// countNote reports how much was listed, for --all across several pages.
func (a *App) countNote(rows, pages int, noun string) {
	if pages <= 1 {
		return
	}
	a.Notef("\n%s across %s.\n", plural(rows, noun), plural(pages, "page"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
