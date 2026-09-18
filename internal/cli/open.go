package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/exitcode"
	"github.com/kurenn/dossier-cli/internal/render"
)

// stdoutTarget is the --out value meaning "write the bytes to stdout".
const stdoutTarget = "-"

// lockWarning is the one line the CLI adds to a wrong PIN. The API says the attempt was
// logged; it does not say that attempts accumulate toward a lock, and a recipient who
// learns that only by hitting it has been told too late.
const lockWarning = "Each wrong attempt is recorded and counts toward a five-attempt lock."

// burnDocumentNote is §8.3's line, added to every 404 from the document endpoint.
//
// Stated as a general fact rather than a claim about this request, because it has to be:
// the API answers an unknown id, a document on someone else's dossier and a document on
// a burn-after-read dossier with one identical 404, deliberately. Asserting which one
// happened would be the CLI inventing a fact it was refused.
const burnDocumentNote = "Documents on a burn-after-read dossier cannot be fetched over the API. " +
	"The holder can share the file another way."

// ambiguousOpen is what §8.2 requires the CLI to say when a `view` fails in transit.
//
// This is the sentence the whole one-request rule exists to make possible. Having sent
// the request and not heard back, the CLI cannot know whether the dossier was opened —
// and for a burn-after-read one, "opened" means "spent". Retrying to find out is the one
// action guaranteed to destroy the evidence, so the CLI stops and says exactly what it
// does and does not know.
const ambiguousOpen = "The request may have reached Dossier. If this was a burn-after-read dossier, " +
	"it may now be consumed. Ask the holder before trying again."

type openOptions struct {
	token    string
	pinStdin bool
	document int
	out      string
	urlOnly  bool
}

func newOpenCmd(app *App) *cobra.Command {
	opts := openOptions{}

	cmd := &cobra.Command{
		Use:   "open TOKEN",
		Short: "Open a dossier someone released to you",
		Long: "Open a dossier someone released to you.\n\n" +
			"This is the recipient side, and the only command that needs no account and no " +
			"token of your own — just the link you were given and the PIN that came with it.\n\n" +
			"The PIN is never an argument. It is read from a hidden prompt, or from stdin with " +
			"--pin-stdin, so it stays out of your shell history and out of ps.\n\n" +
			"Opening is not free. The holder sees that you opened it, and a dossier released " +
			"burn-after-read is consumed by the first open — there is no way to check " +
			"beforehand, so open it when you are ready to read it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.token = args[0]
			return app.runOpen(cmd.Context(), opts, cmd)
		},
	}

	cmd.Flags().BoolVar(&opts.pinStdin, "pin-stdin", false,
		"Read the PIN from stdin instead of prompting, for scripts and pipes")
	cmd.Flags().IntVar(&opts.document, "document", 0,
		"Fetch one released document by id, instead of opening the dossier. Ids come from a previous open")
	cmd.Flags().StringVar(&opts.out, "out", "",
		"Where to write the document. Use - for stdout. Required with --document unless --url-only")
	cmd.Flags().BoolVar(&opts.urlOnly, "url-only", false,
		"Print the signed download URL instead of following it")

	return cmd
}

func (a *App) runOpen(ctx context.Context, opts openOptions, cmd *cobra.Command) error {
	if strings.TrimSpace(opts.token) == "" {
		return Usagef("Give the dossier token from your link, for example: dossier open K7M2P9QRX\n")
	}
	if err := validateOpenFlags(opts, cmd); err != nil {
		return err
	}

	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	// No requireToken, and no token on the wire: this boundary is un-authenticated, and
	// a recipient is not expected to have an account. Only the host is resolved.
	client, err := a.Client(resolution)
	if err != nil {
		return err
	}

	pin, err := a.readPin(opts)
	if err != nil {
		return err
	}

	if cmd.Flags().Changed("document") {
		return a.fetchDocument(ctx, client, opts, pin)
	}
	return a.openDossier(ctx, client, opts, pin)
}

// validateOpenFlags refuses combinations before anything is sent, so a recipient never
// spends an open learning that a flag was wrong.
func validateOpenFlags(opts openOptions, cmd *cobra.Command) error {
	if !cmd.Flags().Changed("document") {
		if opts.out != "" || opts.urlOnly {
			return Usagef("--out and --url-only describe a document, so they need --document ID.\n"+
				"Open the dossier first to see the ids: dossier open %s\n", opts.token)
		}
		return nil
	}

	if opts.document <= 0 {
		return Usagef("--document takes a document id, which is a positive integer.\n" +
			"The ids come from opening the dossier.\n")
	}
	if opts.urlOnly && opts.out != "" {
		return Usagef("--url-only prints the link instead of following it, so it cannot also write to --out.\n")
	}
	if !opts.urlOnly && opts.out == "" {
		// Not defaulted to stdout. These are bytes of a scanned identity document, and a
		// command that dumps them into a terminal by default is one keystroke from a
		// corrupted session and a passport in the scrollback.
		return Usagef("Say where the document should go: --out FILE, or --out - for stdout.\n" +
			"To see the link without downloading it, use --url-only.\n")
	}
	return nil
}

// readPin gets the PIN from stdin or a hidden prompt, and from nowhere else.
func (a *App) readPin(opts openOptions) (string, error) {
	if opts.pinStdin {
		raw, err := io.ReadAll(a.Stdin)
		if err != nil {
			return "", Unexpectedf("could not read the PIN from stdin: %s\n", err)
		}
		pin := strings.TrimSpace(string(raw))
		if pin == "" {
			return "", Usagef("--pin-stdin was given but stdin was empty.\n")
		}
		return pin, nil
	}

	if !a.IsInteractive() {
		return "", Usagef("A PIN is needed and there is no terminal to ask on.\n"+
			"Pipe it in instead: echo $PIN | dossier open %s --pin-stdin\n", opts.token)
	}

	pin, err := a.readSecret("PIN: ")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(pin) == "" {
		return "", Usagef("No PIN entered.\n")
	}
	return strings.TrimSpace(pin), nil
}

// openDossier sends the one POST .../view and renders what comes back.
func (a *App) openDossier(ctx context.Context, client *api.Client, opts openOptions, pin string) error {
	result, err := client.OpenDossier(ctx, opts.token, pin, a.timeout())
	if err != nil {
		return a.classifyOpen(err)
	}

	if a.JSON {
		a.emitRawPage(result.Raw)
		return nil
	}

	a.renderDossier(result.Dossier)
	// Rule 9 cuts both ways: the audit log is honest to the holder about the recipient,
	// so the recipient is told the same thing rather than left to assume privacy.
	a.Notef("This open has been recorded for the holder.\n")
	return nil
}

// classifyOpen maps a failed `view` to its exit code and adds the two sentences the API
// cannot say for itself.
func (a *App) classifyOpen(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		// Transport, not envelope: nothing answered, so the outcome is genuinely unknown.
		// §8.2 — say so, and do not retry.
		return &Error{
			Code:    exitcode.Unexpected,
			Message: fmt.Sprintf("%s\n%s\n", err, ambiguousOpen),
			Err:     err,
		}
	}

	cliErr := FromAPIError(apiErr)
	if apiErr.Code == "pin_incorrect" {
		cliErr.Message = strings.TrimRight(cliErr.Message, "\n") + "\n" + lockWarning + "\n"
	}
	return cliErr
}

// fetchDocument is the --document path, which sends no `view` at all.
//
// §8.2's reasoning: the id can only have come from an earlier open, and a `view` here to
// re-learn it would itself be an open — on a burn-after-read dossier, the open. So this
// goes straight to the document endpoint, and the dossier is never touched.
func (a *App) fetchDocument(ctx context.Context, client *api.Client, opts openOptions, pin string) error {
	link, err := client.DocumentLink(ctx, opts.token, opts.document, pin, a.timeout())
	if err != nil {
		return a.classifyDocument(err)
	}

	if a.JSON {
		a.emitRawPage(link.Raw)
		return nil
	}

	if opts.urlOnly {
		// The one place the URL is printed. It is a bearer capability with about five
		// minutes on it, and anything that lands in scrollback should be there because
		// it was asked for.
		a.Println(link.URL)
		a.Notef("This link carries its own permission and expires in about %s. Treat it like the PIN.\n",
			humaniseSeconds(link.ExpiresIn))
		return nil
	}

	return a.downloadDocument(ctx, link, opts)
}

// classifyDocument maps a failed document fetch, adding §8.3's line to every 404.
func (a *App) classifyDocument(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		// No burn hazard here — the API refuses documents on a burn dossier before it
		// checks anything — so this is an ordinary unreachable-server failure and says so
		// without the ambiguity warning `view` needs.
		return Classify(err)
	}

	cliErr := FromAPIError(apiErr)
	switch apiErr.Code {
	case "not_found":
		cliErr.Message = strings.TrimRight(cliErr.Message, "\n") + "\n" + burnDocumentNote + "\n"
	case "pin_incorrect":
		cliErr.Message = strings.TrimRight(cliErr.Message, "\n") + "\n" + lockWarning + "\n"
	}
	return cliErr
}

// downloadDocument follows the signed URL and writes the bytes where they were asked for.
func (a *App) downloadDocument(ctx context.Context, link *api.DocumentLink, opts openOptions) error {
	if opts.out == stdoutTarget {
		written, err := api.FetchSigned(ctx, a.httpClient(), link.URL, a.Stdout)
		if err != nil {
			return Classify(err)
		}
		a.Notef("%s written to stdout.\n", humaniseBytes(written))
		return nil
	}

	// Written to a temporary file in the same directory and renamed, so an interrupted
	// download leaves no half-file wearing the name of a document someone will open.
	dir := filepath.Dir(opts.out)
	temp, err := os.CreateTemp(dir, ".dossier-download-*")
	if err != nil {
		return Unexpectedf("could not write to %s: %s\n", dir, err)
	}
	tempName := temp.Name()
	defer func() {
		temp.Close()
		os.Remove(tempName)
	}()

	// 0600 from the start: this is an identity document, and the window between creating
	// it world-readable and fixing it up is a window.
	if err := temp.Chmod(0o600); err != nil {
		return Unexpectedf("could not secure %s: %s\n", tempName, err)
	}

	written, err := api.FetchSigned(ctx, a.httpClient(), link.URL, temp)
	if err != nil {
		return Classify(err)
	}
	if err := temp.Close(); err != nil {
		return Unexpectedf("could not finish writing %s: %s\n", tempName, err)
	}
	if err := os.Rename(tempName, opts.out); err != nil {
		return Unexpectedf("could not save %s: %s\n", opts.out, err)
	}

	a.Notef("%s written to %s.\n", humaniseBytes(written), opts.out)
	return nil
}

// httpClient is the transport for signed URLs — deliberately not the api.Client.
func (a *App) httpClient() *http.Client {
	if a.SignedURLClient != nil {
		return a.SignedURLClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// renderDossier writes §8.2's success rendering.
func (a *App) renderDossier(dossier api.Dossier) {
	colors := a.Colors()
	state := render.State(dossier.State)

	block := render.NewBlock(len("NATIONALITY"))
	block.Add("TITLE", dossier.Title)
	block.AddRaw("STATE", colors.StateWord(state))
	block.Add("EXPIRES", derefOr(dossier.ExpiresAt, ""))
	block.Add("HOLDER", dossier.Holder.Name)
	block.Add("NATIONALITY", dossier.Holder.Nationality)
	_ = block.Render(a.Stdout)

	a.Println()
	a.renderReleasedFields(dossier.ReleasedFields)

	if len(dossier.RedactedFields) > 0 {
		a.Println()
		a.renderRedactedFields(dossier.RedactedFields)
	}
	if len(dossier.Documents) > 0 {
		a.Println()
		a.renderDossierDocuments(dossier.Documents)
	}
}

func (a *App) renderReleasedFields(fields []api.ReleasedField) {
	a.Println("RELEASED")
	if len(fields) == 0 {
		// Possible: silent exclusion can empty a release without the mint failing, and a
		// recipient staring at a heading with nothing under it deserves the reason.
		a.Println(render.EmptyCell)
		a.Notef("This dossier released no fields. That can happen when the fields it was minted " +
			"with were withdrawn before you opened it.\n")
		return
	}

	block := render.NewBlock(0)
	for _, field := range fields {
		value := field.Value
		if field.Country != nil && *field.Country != "" {
			value += "  " + *field.Country
		}
		block.Add(field.Label, value)
	}
	_ = block.Render(a.Stdout)
}

// renderRedactedFields draws the bar. The width is the server's mask_length and the bar
// is the only thing drawn, because rule 5 means there is no value here to reveal — not
// hidden, not present. A bar the recipient could widen back into a value would be the
// redaction being decorative.
func (a *App) renderRedactedFields(fields []api.RedactedField) {
	a.Println("WITHHELD")

	block := render.NewBlock(0)
	for _, field := range fields {
		block.Add(field.Label, strings.Repeat("█", field.MaskLength))
	}
	_ = block.Render(a.Stdout)
}

func (a *App) renderDossierDocuments(documents []api.DossierDocument) {
	a.Println("DOCUMENTS")
	table := render.NewTable("ID", "FILENAME", "TYPE")
	for _, document := range documents {
		table.Add(strconv.Itoa(document.ID), document.Filename, document.ContentType)
	}
	_ = table.Render(a.Stdout)
}

func humaniseSeconds(seconds int) string {
	if seconds >= 60 {
		minutes := seconds / 60
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return fmt.Sprintf("%d seconds", seconds)
}
