package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
	"github.com/spf13/cobra"
)

func newSchemaCmd(app *App) *cobra.Command {
	var refresh bool

	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print the API's discovery document",
		Long: "Fetches GET /api/v1/schema — the document the API publishes about itself:\n" +
			"its endpoints, scopes, filter vocabularies, error codes, limits and expiry\n" +
			"rules.\n\n" +
			"This endpoint needs no token, so this is the one command that works on a\n" +
			"machine that has never logged in. It is also what every other command reads\n" +
			"its numbers and vocabularies from, rather than hardcoding them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runSchema(cmd.Context(), refresh)
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Bypass the 24h cache and fetch from the server")
	return cmd
}

func (a *App) runSchema(ctx context.Context, refresh bool) error {
	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	client, err := a.Client(resolution)
	if err != nil {
		return err
	}

	schema, cached, cachedAge, err := a.schema(ctx, client, refresh)
	if err != nil {
		return err
	}

	if a.JSON {
		// Byte-for-byte, exactly as the server sent it. §7.4: the API's shape is already
		// the contract, and re-serialising the decoded struct would publish a second one
		// that has to be kept in step with it forever.
		a.Printf("%s", schema.Raw)
		if !strings.HasSuffix(string(schema.Raw), "\n") {
			a.Printf("\n")
		}
		return nil
	}

	if cached {
		// The age is given, not just the fact of the cache: "cached" alone leaves a
		// holder chasing a stale number with no idea whether it is an hour or a day old.
		a.Notef("Read from the local cache, fetched %s ago. Use --refresh for the server's copy.\n\n",
			humaniseAge(cachedAge))
	}

	a.renderSchema(schema, resolution.Host)
	return nil
}

// schema returns the discovery document, from the cache when it is fresh.
//
// Shared with `login`, which fetches it both to warm the cache and to prove the host is
// actually a Dossier before it sends a token anywhere. Returns whether the copy came from
// the cache so the caller can say so.
func (a *App) schema(ctx context.Context, client *api.Client, refresh bool) (*api.Schema, bool, time.Duration, error) {
	host := client.Host()

	if !refresh {
		if cached, _ := store.LoadSchema(a.Paths, host); cached != nil && cached.Fresh(a.Now()) {
			var schema api.Schema
			if err := json.Unmarshal(cached.Document, &schema); err == nil {
				schema.Raw = cached.Document
				return &schema, true, cached.Age(a.Now()), nil
			}
			// A cache that decodes to nothing useful is ignored rather than repaired.
			// Falling through to the network is always correct; the cache is disposable.
		}
	}

	schema, err := client.FetchSchema(ctx)
	if err != nil {
		return nil, false, 0, Classify(err)
	}

	// A cache write failure is not worth failing the command over: the holder asked for
	// the schema and has it. Saying so on stderr is enough, because a cache that silently
	// never persists would show up later as unexplained slowness.
	if err := store.SaveSchema(a.Paths, host, schema.Raw); err != nil {
		a.Notef("Could not cache the schema: %s\n", err)
	}
	return schema, false, 0, nil
}

// humaniseAge renders a cache age the way the product renders a countdown: whole units,
// largest first, never "0h 47m".
func humaniseAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "less than a minute"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	default:
		return fmt.Sprintf("%dh %02dm", int(age.Hours()), int(age.Minutes())%60)
	}
}

func (a *App) renderSchema(schema *api.Schema, host string) {
	block := render.NewBlock(10).Add("HOST", host)
	_ = block.Render(a.Stdout)
	a.Println()

	if len(schema.Endpoints) > 0 {
		a.Println("ENDPOINTS")
		widest := 0
		for _, endpoint := range schema.Endpoints {
			if n := len(endpoint.Method); n > widest {
				widest = n
			}
		}
		for _, endpoint := range schema.Endpoints {
			scope := endpoint.Scope
			if scope == "" {
				// The schema endpoint itself, and the recipient routes: no scope because
				// no token. Naming that explicitly is more useful than an empty column.
				scope = "(no token)"
			}
			a.Printf("  %-*s  %-42s  %s\n", widest, endpoint.Method, endpoint.Path, scope)
		}
		a.Println()
	}

	vocab := render.NewBlock(18)
	if len(schema.Scopes) > 0 {
		vocab.Add("SCOPES", strings.Join(schema.Scopes, " "))
	}
	if len(schema.FieldCategories) > 0 {
		vocab.Add("FIELD CATEGORIES", strings.Join(schema.FieldCategories, " "))
	}
	if len(schema.FieldStatuses) > 0 {
		vocab.Add("FIELD STATUSES", strings.Join(schema.FieldStatuses, " "))
	}
	if len(schema.ShareStates) > 0 {
		vocab.Add("SHARE STATES", strings.Join(schema.ShareStates, " "))
	}
	if vocab.Width() > 0 && len(vocab.String()) > 0 {
		_ = vocab.Render(a.Stdout)
		a.Println()
	}

	if len(schema.Limits) > 0 {
		a.Println("LIMITS")
		for _, line := range flattenLimits(schema.Limits) {
			a.Printf("  %s\n", line)
		}
		a.Println()
	}

	// Expiry last, and the no-expiry exception last within it, set apart by a blank line
	// and rendered with the server's own label and note. Rule 2 is that expiry is
	// required and "no expiry" is the labelled exception — presenting the two as adjacent
	// equal options is the specific thing the product refuses to do, so the client must
	// not flatten them either.
	if schema.Expiry.Required || len(schema.Expiry.Presets) > 0 {
		expiry := render.NewBlock(10).
			AddRaw("REQUIRED", boolWord(schema.Expiry.Required)).
			Add("PRESETS", strings.Join(schema.Expiry.Presets, " "))
		a.Println("EXPIRY")
		for _, line := range strings.Split(strings.TrimRight(expiry.String(), "\n"), "\n") {
			a.Printf("  %s\n", line)
		}
		if schema.Expiry.Note != "" {
			a.Printf("\n  %s\n", schema.Expiry.Note)
		}
		if schema.Expiry.NoExpiry.Label != "" {
			a.Printf("\n  %s — %s\n", schema.Expiry.NoExpiry.Label, schema.Expiry.NoExpiry.Note)
		}
		a.Println()
	}

	// The one place this binary admits it may be behind the server. A code the CLI cannot
	// map would otherwise surface much later as an unexplained exit 1 in the middle of
	// someone's script.
	if unmapped := exitcode.Unmapped(schema.ErrorCodeStrings()); len(unmapped) > 0 {
		a.Notef(
			"This server publishes %d error code(s) this version of dossier does not know:\n  %s\n"+
				"They will exit 1 rather than a specific status. Upgrade dossier to map them.\n",
			len(unmapped), strings.Join(unmapped, " "),
		)
	}
}

// flattenLimits renders the nested limits object as sorted, aligned `path  value` lines.
//
// Sorted because Go randomises map iteration, and output that reorders itself between runs
// cannot be diffed, golden-tested, or trusted by anyone comparing two machines.
//
// The width is computed from the data rather than fixed, per the list design: several of
// the API's own limit keys are human phrases ("failed authentication, per IP") and are
// longer than any column width guessed in advance, so a fixed pad silently stops aligning
// exactly where the values matter most.
//
// Walked generically because limits is a nested object of numbers and strings rather than
// a fixed struct — naming the keys here would mean editing this client every time the API
// publishes another one, which is the drift the schema exists to prevent.
func flattenLimits(limits map[string]any) []string {
	type entry struct {
		path  string
		value string
	}
	var entries []entry

	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, typed[key])
			}
		case float64:
			// JSON numbers decode to float64. Integers print without a decimal point
			// because every limit here is a count or a byte size, and "26214400.0" reads
			// as a measurement rather than a cap.
			if typed == float64(int64(typed)) {
				entries = append(entries, entry{prefix, strconv.FormatInt(int64(typed), 10)})
			} else {
				entries = append(entries, entry{prefix, fmt.Sprintf("%v", typed)})
			}
		default:
			entries = append(entries, entry{prefix, fmt.Sprintf("%v", typed)})
		}
	}
	walk("", limits)

	width := 0
	for _, item := range entries {
		if n := utf8.RuneCountInString(item.path); n > width {
			width = n
		}
	}

	lines := make([]string, 0, len(entries))
	for _, item := range entries {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(item.path))
		lines = append(lines, item.path+pad+"  "+item.value)
	}
	return lines
}

func boolWord(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
