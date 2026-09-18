package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/store"
)

// Shell completion, with one rule running through all of it: **a tab press makes no
// network request.**
//
// That rule is not fussiness. Completion fires on a keystroke, often several times a
// second, and against this API every request costs something real — a unit of a published
// rate-limit budget, a row in the holder's audit trail, and on one endpoint an open that
// cannot be taken back. A shell that quietly spent any of those while someone was still
// deciding what to type would be indefensible, and the failure would be invisible: the
// completions would look fine.
//
// So everything below reads local state or nothing. The schema is taken from the cache
// and never fetched; a cold cache completes nothing rather than filling it. Profile names
// come from the credentials file. Nothing completes a dossier token, a field value or a
// PIN, because those are either secrets or not ours to enumerate.
//
// TestCompletionNeverTouchesTheNetwork holds the line.

// registerCompletions attaches the local-only completion functions to the command tree.
func registerCompletions(app *App, root *cobra.Command) {
	register := func(cmd *cobra.Command, flag string, fn func() []string) {
		// cmd.Flag, not cmd.Flags().Lookup: the latter does not see a persistent flag
		// until the flag sets have been merged, which happens at parse time. `--profile`
		// is persistent, so the guard silently skipped it and `--profile <TAB>` fell
		// back to a file listing with every completion test still passing.
		if cmd == nil || cmd.Flag(flag) == nil {
			return
		}
		_ = cmd.RegisterFlagCompletionFunc(flag,
			func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
				// NoFileComp throughout: every one of these flags takes a value from a
				// closed set, and offering the working directory's file listing when the
				// set is empty is worse than offering nothing.
				return fn(), cobra.ShellCompDirectiveNoFileComp
			})
	}

	register(root, "profile", app.completeProfiles)

	if fields := find(root, "fields"); fields != nil {
		register(find(fields, "list"), "status", app.completeFieldStatuses)
		register(find(fields, "create"), "category", app.completeFieldCategories)
	}
	if shares := find(root, "shares"); shares != nil {
		register(find(shares, "list"), "state", app.completeShareStates)
	}
}

// find returns a direct subcommand by its first word, or nil.
func find(parent *cobra.Command, name string) *cobra.Command {
	for _, candidate := range parent.Commands() {
		if candidate.Name() == name {
			return candidate
		}
	}
	return nil
}

// completeProfiles offers the profile names actually stored on this machine.
//
// A permission error is swallowed rather than surfaced: the credentials file being
// world-readable is a refusal worth making loudly when a command runs, and worth making
// silently when a shell is drawing a menu. Printing it here would corrupt the completion
// output with prose the shell would try to insert.
func (a *App) completeProfiles() []string {
	creds, err := store.LoadCredentials(a.Paths)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(creds.Profiles))
	for name := range creds.Profiles {
		names = append(names, name)
	}
	return names
}

func (a *App) completeFieldStatuses() []string {
	schema := a.cachedSchema()
	if schema == nil {
		return nil
	}
	return schema.FieldStatuses
}

func (a *App) completeFieldCategories() []string {
	schema := a.cachedSchema()
	if schema == nil {
		return nil
	}
	return schema.FieldCategories
}

func (a *App) completeShareStates() []string {
	schema := a.cachedSchema()
	if schema == nil {
		return nil
	}
	return schema.ShareStates
}

// cachedSchema reads the discovery document from disk and never from the network.
//
// Staleness is tolerated here, deliberately, where the commands themselves refuse it. A
// completion list is a suggestion the server validates anyway — offering a status that
// has since been retired costs one clear error message, whereas refusing to complete
// because the cache turned 24 hours old costs the feature. The commands still check the
// live vocabulary, so a stale suggestion cannot become a stale request.
func (a *App) cachedSchema() *api.Schema {
	resolution, err := a.Resolve()
	if err != nil {
		return nil
	}
	cached, err := store.LoadSchema(a.Paths, resolution.Host)
	if err != nil || cached == nil {
		return nil
	}

	var schema api.Schema
	if err := json.Unmarshal(cached.Document, &schema); err != nil {
		return nil
	}
	return &schema
}
