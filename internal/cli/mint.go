package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/kurenn/dossier-cli/internal/store"
	"github.com/spf13/cobra"
)

// Bounds from the plan's §6.3. Named so the retry logic reads as the table it implements.
const (
	// rateLimitWaits is how many times a 429 is waited out before giving up. The API's
	// Retry-After here is a full minute, so this is up to three minutes of patience — a
	// deliberate choice for the one command where giving up costs the holder a repeat of
	// the whole flow.
	rateLimitWaits = 3

	// rateLimitWaitCap bounds any single wait, so a proxy that rewrote Retry-After into
	// something absurd cannot park the CLI for an hour.
	rateLimitWaitCap = 60 * time.Second

	// inFlightAttempts is the 1s/2s/4s backoff for a `409 request_in_flight` that has no
	// 5xx recorded against its key — a genuinely concurrent or stale-racing claim, which
	// resolves on its own or not at all.
	inFlightAttempts = 3
)

type mintOptions struct {
	title       string
	fieldIDs    []int
	documentIDs []int
	recipients  []string

	expires  string
	noExpiry bool

	burnAfterRead         bool
	allowDocumentDownload bool
	noNotifyOnOpen        bool

	key    string
	resume string
	yes    bool
}

func newSharesMintCmd(app *App) *cobra.Command {
	var opts mintOptions

	cmd := &cobra.Command{
		Use:   "mint",
		Short: "Release a dossier to a recipient, with a deadline",
		Long: "Mints a dossier: a named subset of your vault, released to one recipient, that\n" +
			"stops working on a deadline you set.\n\n" +
			"An expiry is required. Pass --expires or --no-expiry; with neither, a terminal\n" +
			"gets the picker and a script is refused before anything is sent. There is no\n" +
			"default deadline and this command will not construct one.\n\n" +
			"Each recipient becomes its own dossier with its own link and its own PIN,\n" +
			"joined in one batch. The PIN is returned once, in the response to this command,\n" +
			"and cannot be recovered afterwards by any route.\n\n" +
			"Every attempt carries an idempotency key, recorded locally before the request\n" +
			"is sent. If a mint fails in a way that leaves the outcome unknown, the key is\n" +
			"kept and `--resume` finishes it — the server answers a repeat of the same\n" +
			"request from its record of the first, so resuming cannot mint twice.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.resume != "" {
				return app.runMintResume(cmd.Context(), opts)
			}
			return app.runMint(cmd.Context(), opts, cmd)
		},
	}

	cmd.Flags().StringVar(&opts.title, "title", "", "What this dossier is for, shown to the recipient")
	cmd.Flags().IntSliceVar(&opts.fieldIDs, "field", nil, "A field id to release; repeat for several (see: dossier fields list)")
	cmd.Flags().IntSliceVar(&opts.documentIDs, "document", nil, "Attach only these document ids; omit to attach every document on the released fields")
	cmd.Flags().StringArrayVar(&opts.recipients, "to", nil, "Recipient, as \"Name <email>\" or just a name; repeat for a batch")

	cmd.Flags().StringVar(&opts.expires, "expires", "", "When it stops working: PT24H, 7d, or 2026-12-01T09:00:00Z")
	cmd.Flags().BoolVar(&opts.noExpiry, "no-expiry", false, "The labelled exception: works until you revoke it by hand")

	cmd.Flags().BoolVar(&opts.burnAfterRead, "burn-after-read", false, "The dossier stops after one open")
	cmd.Flags().BoolVar(&opts.allowDocumentDownload, "allow-document-download", false, "Let the recipient download documents, not only view them")
	cmd.Flags().BoolVar(&opts.noNotifyOnOpen, "no-notify-on-open", false, "Do not notify you when it is opened")

	cmd.Flags().StringVar(&opts.key, "idempotency-key", "", "Use this key instead of a generated one")
	cmd.Flags().StringVar(&opts.resume, "resume", "", "Finish an unresolved mint by its key")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Skip the confirmation prompt (it does not skip the expiry requirement)")

	return cmd
}

func (a *App) runMint(ctx context.Context, opts mintOptions, cmd *cobra.Command) error {
	if len(opts.fieldIDs) == 0 {
		return Usagef("Nothing to release. Name at least one field with --field.\n\n    dossier fields list\n")
	}
	if opts.expires != "" && opts.noExpiry {
		return Usagef("Choose one of --expires and --no-expiry, not both.\n")
	}

	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	if err := a.requireToken(resolution); err != nil {
		return err
	}
	client, err := a.mintClient(resolution)
	if err != nil {
		return err
	}

	schema, _, _, err := a.schema(ctx, client, false)
	if err != nil {
		return err
	}

	expiry, err := a.resolveExpiry(opts, schema)
	if err != nil {
		return err
	}

	recipients, err := parseRecipients(opts.recipients)
	if err != nil {
		return err
	}

	input := api.MintInput{
		Title:      opts.title,
		FieldIDs:   opts.fieldIDs,
		Recipients: recipients,
		ExpiresAt:  expiry.At,
	}
	// Changed(), not the value: the API leaves an omitted boolean at the column's own
	// default, so sending `false` because a flag defaulted to false would be the client
	// overriding a server default it was never asked to touch.
	if cmd.Flags().Changed("document") {
		ids := opts.documentIDs
		if ids == nil {
			ids = []int{}
		}
		input.DocumentIDs = &ids
	}
	if cmd.Flags().Changed("burn-after-read") {
		input.BurnAfterRead = &opts.burnAfterRead
	}
	if cmd.Flags().Changed("allow-document-download") {
		input.AllowDocumentDownload = &opts.allowDocumentDownload
	}
	if cmd.Flags().Changed("no-notify-on-open") {
		notify := !opts.noNotifyOnOpen
		input.NotifyOnOpen = &notify
	}

	body, err := api.BuildMintBody(input)
	if err != nil {
		return Unexpectedf("%s\n", err)
	}

	if !opts.yes {
		if err := a.confirmMint(input, expiry, recipients); err != nil {
			return err
		}
	}

	key := opts.key
	if key == "" {
		key = newIdempotencyKey()
	}
	if !store.ValidKey(key) {
		return Usagef(
			"%q cannot be used as an idempotency key.\n\n"+
				"Use letters, digits, dashes and underscores, up to 255 bytes. The key names a\n"+
				"file in the local ledger, so it cannot contain a path.\n", key)
	}

	// Written before the request, not after. The entire value of the ledger is in the
	// window between "sent" and "answered": an entry created after a reply would be
	// missing for exactly the failures it exists to recover from.
	entry := &store.MintEntry{
		Key:         key,
		Body:        body,
		Digest:      api.BodyDigest(body),
		Host:        resolution.Host,
		ProfileName: resolution.ProfileName,
		CreatedAt:   a.Now(),
	}
	if err := a.Paths.SaveMint(entry); err != nil {
		return Unexpectedf("%s\n", err)
	}

	return a.sendMint(ctx, client, entry, input.Title)
}

func (a *App) runMintResume(ctx context.Context, opts mintOptions) error {
	if !store.ValidKey(opts.resume) {
		return Usagef("%q is not a key this ledger could have written.\n", opts.resume)
	}

	entry, err := a.Paths.LoadMint(opts.resume)
	if errors.Is(err, store.ErrNoEntry) {
		return Usagef(
			"No unresolved mint under the key %s.\n\n"+
				"A key is recorded before its request is sent and removed once the mint is\n"+
				"confirmed, so nothing here means there is nothing outstanding to finish.\n",
			opts.resume)
	}
	if err != nil {
		return Unexpectedf("%s\n", err)
	}

	if api.BodyDigest(entry.Body) != entry.Digest {
		return Unexpectedf(
			"The ledger entry for %s does not match its own checksum, so the bytes it would\n"+
				"resend are not the bytes the key was first used with. Resending them would\n"+
				"earn a permanent rejection for this key.\n\n"+
				"Run `dossier shares list` to see whether the mint landed.\n", entry.Key)
	}

	// The 24 h refusal, and the one place it really matters. Past the server's claim TTL a
	// replay is not a replay: the claim may have been reclaimed, so the same key would
	// mint a second batch. See store.LedgerTTL.
	if entry.Stale(a.Now()) {
		return &Error{
			Code: exitcode.Usage,
			Message: fmt.Sprintf(
				"That mint is %s old and cannot be resumed.\n\n"+
					"Dossier holds an idempotency claim for 24 hours. Past that, sending the same\n"+
					"key again is a new mint rather than a replay of the old one, so this refuses\n"+
					"rather than risk releasing a second dossier.\n\n"+
					"Run `dossier shares list` to see whether the original landed. If it did not,\n"+
					"mint again — a new run generates a new key.\n",
				humaniseDuration(entry.Age(a.Now()))),
		}
	}

	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	if err := a.requireToken(resolution); err != nil {
		return err
	}
	if entry.Host != "" && entry.Host != resolution.Host {
		return Usagef(
			"That mint was sent to %s and this profile points at %s.\n\n"+
				"An idempotency key belongs to the vault it was claimed on, so resuming against\n"+
				"a different host would be a new mint. Use --host %s, or --profile %s.\n",
			entry.Host, resolution.Host, entry.Host, entry.ProfileName)
	}

	client, err := a.mintClient(resolution)
	if err != nil {
		return err
	}

	a.Notef("Resending the same request under key %s.\n\n", entry.Key)

	// The title is not in the 201 — the API does not echo it — so the block's TITLE row
	// is always the one that was sent. On a resume that means reading it back out of the
	// stored body, which is the only record of it this side has.
	return a.sendMint(ctx, client, entry, titleFromBody(entry.Body))
}

// sendMint is the retry table of §6.3, and the only place a mint request is issued.
//
// Every branch either resolves the key or deliberately keeps the ledger entry. The ones
// that keep it are the ones where the outcome is unknown, and the distinction that makes
// them decidable is whether a 5xx has already been seen under this key — which is why the
// entry is written back to disk the moment one is.
func (a *App) sendMint(ctx context.Context, client *api.Client, entry *store.MintEntry, title string) error {
	var (
		waits           int
		inFlightTries   int
		retriedAfter5xx bool
	)

	for {
		entry.Attempts++
		result, err := client.Mint(ctx, entry.Key, entry.Body, a.timeout())

		if err == nil {
			// The key is resolved. The ledger entry is removed only after the block has
			// been printed, so a crash between the two leaves something to resume rather
			// than a share whose PIN was never shown.
			if printErr := a.renderMint(result, title); printErr != nil {
				return printErr
			}
			if err := a.Paths.DeleteMint(entry.Key); err != nil {
				a.Notef("\nThe mint succeeded, but its ledger entry could not be removed: %s\n", err)
			}
			return nil
		}

		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			// Transport failure after the body was sent. The outcome is unknown, and
			// nothing on this path releases the server's claim — so the same key is
			// answerable from the server's own record, which is what makes this
			// recoverable rather than merely failed.
			_ = a.Paths.SaveMint(entry)
			return &Error{
				Code: exitcode.Unexpected,
				Message: fmt.Sprintf(
					"%s\n\nThe request may have reached Dossier. Run\n\n    dossier shares mint --resume %s\n\n"+
						"to find out — it sends the same request under the same key, which Dossier\n"+
						"answers from the first one.\n", err, entry.Key),
				Err: err,
			}
		}

		switch apiErr.Code {
		case "rate_limited":
			// The key was not claimed, so this is free to repeat with the same bytes.
			if a.NoWait {
				_ = a.Paths.SaveMint(entry)
				return a.tryLater(apiErr, entry, "--no-wait was set, so nothing was waited out.")
			}
			if waits >= rateLimitWaits {
				_ = a.Paths.SaveMint(entry)
				return a.tryLater(apiErr, entry,
					fmt.Sprintf("Waited %d times without getting through.", waits))
			}
			waits++
			wait := time.Duration(apiErr.RetryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			if wait > rateLimitWaitCap {
				wait = rateLimitWaitCap
			}
			a.Notef("Rate limited. Waiting %s before trying again (%d of %d).\n",
				humaniseDuration(wait), waits, rateLimitWaits)
			a.sleep(wait)

		case "request_in_flight":
			if entry.SawServerError() {
				// The case that never resolves itself: the batch was written but the
				// response could not be stored, so the claim stays claimed until its TTL.
				// A backoff loop here would spin for 24 hours and then mint again.
				_ = a.Paths.SaveMint(entry)
				return &Error{
					Code: exitcode.MintUnconfirmed,
					Message: "Dossier has a mint under way for this request and cannot confirm it.\n\n" +
						"A batch matching what you sent may already exist — run `dossier shares list`\n" +
						"and check before minting again.\n",
					Err: apiErr,
				}
			}
			if inFlightTries >= inFlightAttempts {
				_ = a.Paths.SaveMint(entry)
				return a.tryLater(apiErr, entry, "The claim was still in flight after three attempts.")
			}
			backoff := time.Duration(1<<inFlightTries) * time.Second
			inFlightTries++
			a.Notef("Another attempt under this key is still in flight. Retrying in %s (%d of %d).\n",
				humaniseDuration(backoff), inFlightTries, inFlightAttempts)
			a.sleep(backoff)

		case "idempotency_key_reused":
			// Not retried. The bytes differ from a previous send under this key, which the
			// ledger makes nearly impossible from this CLI alone — so the message names
			// the one way it realistically happens.
			return &Error{
				Code: apiErr.ExitCode(),
				Message: apiErr.Render() + "\n" +
					"This key has already been used for a different request. The CLI records the\n" +
					"exact bytes it sends, so this almost always means the key was supplied by hand\n" +
					"with --idempotency-key. Run again without it and a new key is generated.\n",
				Err: apiErr,
			}

		default:
			if apiErr.Status >= 500 {
				if retriedAfter5xx {
					// Never a second retry. One is what distinguishes the two cases; more
					// is just hammering a server that is already unwell.
					_ = a.Paths.SaveMint(entry)
					return &Error{
						Code:    exitcode.MintUnconfirmed,
						Message: apiErr.Render() + "\n" + unconfirmedAdvice(entry.Key),
						Err:     apiErr,
					}
				}
				// Recorded before the retry, because the retry's *answer* is only
				// interpretable in the light of this: a 409 after a 5xx is the claim that
				// will never resolve, and without this flag it looks like one that will.
				at := a.Now()
				entry.ServerErrorAt = &at
				if err := a.Paths.SaveMint(entry); err != nil {
					return Unexpectedf("%s\n", err)
				}
				retriedAfter5xx = true
				a.Notef("Dossier returned %d, which does not say whether the dossier was minted.\n"+
					"Retrying once under the same key; that answer is what tells the two apart.\n\n",
					apiErr.Status)

			} else {
				// Every remaining refusal is the server having decided: a 422, a 403, a
				// 404. Nothing was minted and a corrected request needs a new key, so the
				// entry is removed — leaving it would offer a resume that could only
				// reproduce the same rejection.
				_ = a.Paths.DeleteMint(entry.Key)
				return FromAPIError(apiErr)
			}
		}
	}
}

// tryLater is the exit-8 outcome: nothing was claimed, so trying again later is safe.
func (a *App) tryLater(apiErr *api.Error, entry *store.MintEntry, why string) error {
	return &Error{
		Code: exitcode.TryLater,
		Message: apiErr.Render() + "\n" + why + "\n\n" +
			"Nothing was minted and the key was not spent. Finish it with\n\n" +
			"    dossier shares mint --resume " + entry.Key + "\n",
		Err: apiErr,
	}
}

func unconfirmedAdvice(key string) string {
	return "Whether the dossier was minted is unknown, and a second attempt under a new key\n" +
		"would risk releasing it twice.\n\n" +
		"Run `dossier shares list` to check, or finish this one with\n\n" +
		"    dossier shares mint --resume " + key + "\n"
}

// resolveExpiry applies §6.1: exactly one of the two flags, the picker in a terminal, and
// a refusal quoting the API's own copy in a script.
func (a *App) resolveExpiry(opts mintOptions, schema *api.Schema) (Expiry, error) {
	switch {
	case opts.noExpiry:
		return NoExpiryChosen(), nil
	case opts.expires != "":
		return ParseExpires(opts.expires, a.Now(), schema.Expiry.Presets)
	case a.interactive():
		return a.pickExpiry(schema, a.Now())
	}

	// Not a terminal and neither flag given. Refused before any request, in the API's own
	// words — its message and hint are the product's argument for why this is required,
	// and paraphrasing them here would be a second, weaker version of it.
	message, hint := expiryRequiredCopy(schema)
	refusal := message + "\n"
	if hint != "" {
		// Absent from a schema cached before the API began publishing hints, and from any
		// vault still running that version. The message alone is the product's argument;
		// the hint is the remedy, and a missing one must not leave a blank line where a
		// sentence should be.
		refusal += hint + "\n"
	}
	return Expiry{}, &Error{
		Code: exitcode.Usage,
		Message: refusal +
			"\nPass --expires, or --no-expiry for the labelled exception. Nothing was sent.\n",
	}
}

// expiryRequiredCopy finds the API's own expiry_required copy in the schema, so the
// refusal is the server's sentence rather than one written here.
func expiryRequiredCopy(schema *api.Schema) (message, hint string) {
	for _, row := range schema.ErrorCodes {
		if row.Code == "expiry_required" {
			return row.Message, row.Hint
		}
	}
	// The schema did not publish it. Falling back to the product rule stated plainly is
	// better than saying nothing, and this is the only sentence in the command not taken
	// from the server.
	return "Expiry is required.", "Pass --expires with a duration or a timestamp, or --no-expiry."
}

// confirmMint shows what is about to be released and asks once.
//
// Non-interactive runs skip it rather than block: a script that named its fields, its
// recipients and its expiry has already stated its intent, and the thing that must never
// be skipped — the expiry — was refused earlier if absent. --yes skips it for a terminal.
func (a *App) confirmMint(input api.MintInput, expiry Expiry, recipients []api.Recipient) error {
	if !a.interactive() {
		return nil
	}

	block := render.NewBlock(len("RECIPIENTS"))
	block.Add("TITLE", input.Title)
	block.Add("FIELDS", strconv.Itoa(len(input.FieldIDs)))
	block.Add("RECIPIENTS", recipientSummary(recipients))
	if expiry.NoExpiry {
		block.Add("EXPIRES", NoExpiry)
	} else {
		block.Add("EXPIRES", expiry.ISO())
	}
	if input.BurnAfterRead != nil && *input.BurnAfterRead {
		block.Add("BURN", "stops after one open")
	}
	_ = block.Render(a.Stderr)

	answer, err := a.readLine("\nMint this dossier? [y/N] ")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") {
		return &Error{Code: exitcode.Usage, Message: "Nothing was minted.\n"}
	}
	fmt.Fprintln(a.Stderr)
	return nil
}

func recipientSummary(recipients []api.Recipient) string {
	if len(recipients) == 0 {
		return ""
	}
	parts := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		parts = append(parts, recipient.Display())
	}
	return strings.Join(parts, ", ")
}

// parseRecipients turns each --to into a {label, email}.
//
// "Marisol Vega <marisol@example.com>" is the form a holder already knows from every mail
// client. A bare name is allowed because the API allows it — and that case is exactly the
// one where the CLI's output is the only copy of the PIN, which the handover block says.
func parseRecipients(values []string) ([]api.Recipient, error) {
	recipients := make([]api.Recipient, 0, len(values))

	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, Usagef("--to needs a name, an email, or \"Name <email>\".\n")
		}

		open := strings.LastIndex(trimmed, "<")
		if open >= 0 && strings.HasSuffix(trimmed, ">") {
			label := strings.TrimSpace(trimmed[:open])
			email := strings.TrimSpace(trimmed[open+1 : len(trimmed)-1])
			if email == "" {
				return nil, Usagef("%q has empty angle brackets.\n", value)
			}
			recipients = append(recipients, api.Recipient{Label: label, Email: email})
			continue
		}

		// A bare value containing an @ is an address; anything else is a label. Not
		// validated beyond that: the API decides what an acceptable address is, and a
		// client with its own opinion would refuse things the server accepts.
		if strings.Contains(trimmed, "@") && !strings.Contains(trimmed, " ") {
			recipients = append(recipients, api.Recipient{Email: trimmed})
			continue
		}
		recipients = append(recipients, api.Recipient{Label: trimmed})
	}
	return recipients, nil
}

// renderMint prints the handover block of §6.4, one per share in the batch.
func (a *App) renderMint(result *api.MintResult, title string) error {
	if a.JSON {
		// The PIN passes through. §6.4: that is the holder's decision when they ask for
		// --json, and the CLI does not redact a response the API chose to return.
		a.emitRawPage(result.Raw)
		return nil
	}

	a.Notef("Minted.\n\n")

	for i, share := range result.Shares {
		if i > 0 {
			a.Println()
		}
		a.renderHandover(share, title)
	}

	if len(result.Shares) > 1 {
		a.Notef("\n%d dossiers, one batch (%s). Each has its own link and its own PIN.\n",
			len(result.Shares), result.BatchID)
	}
	return nil
}

func (a *App) renderHandover(share api.MintedShare, title string) {
	colors := a.Colors()
	state := render.State(share.State)

	block := render.NewBlock(len("RECIPIENT"))
	block.Add("TITLE", title)
	block.Add("TOKEN", share.Token)
	block.Add("URL", share.URL)
	block.Add("RECIPIENT", handoverRecipient(share.Recipient))
	block.AddRaw("STATE", colors.StateWord(state))
	if share.ExpiresAt == nil {
		block.Add("EXPIRES", NoExpiry)
	} else {
		block.Add("EXPIRES", share.ExpiresAt.UTC().Format(time.RFC3339))
	}
	block.Add("FIELDS", strconv.Itoa(share.FieldCount))

	// "address on file" or "none", never yes/no. email_queued is exactly whether an
	// address was present, and the API has no delivery signal to give — so a row reading
	// "yes" would tell a holder a mail arrived that the server cannot see.
	block.Add("EMAILED", emailedText(share.EmailQueued))

	block.Blank()
	block.Add("PIN", pinText(share))
	_ = block.Render(a.Stdout)

	a.Notef("\n%s\n", handoverSentence(share))
}

func emailedText(queued bool) string {
	if queued {
		return "address on file"
	}
	return "none"
}

// pinText renders the PIN with the phrase the product uses for a secret shown once — the
// same words the Settings modal uses for an API token.
func pinText(share api.MintedShare) string {
	if !share.PINShownOnce {
		return share.PIN
	}
	return share.PIN + "          shown once"
}

// handoverSentence is the closing prose of §6.4, and which of the two it is depends on
// whether the API had an address to send to.
//
// Neither is hard-wrapped, unlike most prose in this CLI. The first embeds a recipient
// name, and a name's width is not knowable — so a wrap chosen to look right for "Marisol
// Vega" is wrong for "Dr. Aleksandra Kowalczyk-Nowakowska" and worse than no wrap at all.
// The terminal is better at this than a literal newline, and the second sentence follows
// the first rather than being wrapped differently for consistency's sake.
func handoverSentence(share api.MintedShare) string {
	if share.EmailQueued {
		return share.Recipient.Display() +
			" has been sent the link and this PIN. Do not send the PIN again by another channel."
	}
	return "This is the only copy of the PIN. Hand it to the recipient yourself, by a different channel from the link."
}

// handoverRecipient renders both halves, unlike Recipient.Display.
//
// The block in §6.4 shows "Marisol Vega  <marisol@example.com>", and here that is worth a
// column where in a list it would not be: this is the moment a holder checks they are
// about to hand a dossier to the right person, and a label alone does not let them. The
// address is what the issuance email went to.
func handoverRecipient(recipient api.Recipient) string {
	switch {
	case recipient.Label != "" && recipient.Email != "":
		return recipient.Label + "  <" + recipient.Email + ">"
	case recipient.Label != "":
		return recipient.Label
	default:
		return recipient.Email
	}
}

// titleFromBody reads the title back out of a stored mint body.
//
// Only reached on --resume. A failure is not worth reporting: the title is prose on one
// row of the block, and refusing to print a confirmed mint because its title could not be
// recovered would be the wrong trade by a wide margin.
func titleFromBody(body []byte) string {
	var decoded struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(body, &decoded)
	return decoded.Title
}

// newIdempotencyKey generates a UUIDv4.
//
// Hand-rolled rather than taking a dependency: this is the only place the CLI needs one,
// crypto/rand is the same source any library would use, and a vault tool has a standing
// reason to keep the set of third-party code it ships small.
//
// crypto/rand.Read cannot fail on any platform this builds for — since Go 1.24 it panics
// rather than returning an error — so there is no error path to handle here. The two
// masked bytes are the version and variant bits RFC 9562 requires; without them the value
// is still random but is not a v4 UUID, and something downstream that parses it would be
// right to reject it.
func newIdempotencyKey() string {
	var raw [16]byte
	rand.Read(raw[:])

	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	hex := hex.EncodeToString(raw[:])
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// sleep is the injection point for every wait in the retry table, so the tests that drive
// it do not take three minutes.
func (a *App) sleep(d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
		return
	}
	time.Sleep(d)
}

// mintClient is the transport for a mint.
//
// NoWait is forced on at the transport level regardless of the --no-wait flag, so that the
// 429 arrives here as an error rather than being slept on inside Client.Do. That is not a
// duplicate of the flag: §6.3 gives the mint its own bounded patience (three waits, capped
// at a minute each) and its own prose while it waits, and a transport that had already
// slept would make both unreachable. The flag is still honoured — it is read in sendMint,
// where it exits 8 immediately.
func (a *App) mintClient(resolution store.Resolution) (*api.Client, error) {
	return a.Client(resolution, api.WithNoWait(true))
}

// humaniseDuration renders a wait or an age the way the prose around it reads.
func humaniseDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes == 0 {
			return strconv.Itoa(hours) + "h"
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
}
