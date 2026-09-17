package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/spf13/cobra"
)

func newDocumentsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "documents",
		Short: "The scans attached to your fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDocumentsAttachCmd(app))
	return cmd
}

func newDocumentsAttachCmd(app *App) *cobra.Command {
	var contentType string

	cmd := &cobra.Command{
		Use:   "attach FIELD_ID PATH",
		Short: "Attach a scan to one of your fields",
		Long: "Uploads one file to one field. One file per call — attach a second page by\n" +
			"running it again.\n\n" +
			"The file's size is checked here first, against the cap the API publishes in its\n" +
			"schema, so an oversized file is refused before it is uploaded rather than after.\n" +
			"Two things cannot be checked here: the content-type allow-list, which the schema\n" +
			"does not publish, and the whole-vault storage cap, which no endpoint reports the\n" +
			"balance of. Both come back from the server if they are hit.\n\n" +
			"The content type is declared from the file's extension. The server allow-lists\n" +
			"on that declaration rather than inspecting the bytes, so --content-type is how\n" +
			"you send a file whose name does not match what it is.\n\n" +
			"A failure you cannot read is never retried. This endpoint has no\n" +
			"Idempotency-Key and nothing deduplicates an upload, so a second attempt after a\n" +
			"timeout does not replace the first — it attaches a second copy.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runDocumentsAttach(cmd.Context(), args[0], args[1], contentType)
		},
	}

	cmd.Flags().StringVar(&contentType, "content-type", "", "Declare the type explicitly instead of deriving it from the extension")
	return cmd
}

func (a *App) runDocumentsAttach(ctx context.Context, idArg, path, contentTypeFlag string) error {
	fieldID, err := strconv.Atoi(idArg)
	if err != nil || fieldID <= 0 {
		return Usagef("%q is not a field id. Run `dossier fields list` to find one.\n", idArg)
	}

	info, err := os.Stat(path)
	if err != nil {
		return Usagef("Could not read %s: %s\n", path, err)
	}
	if info.IsDir() {
		return Usagef("%s is a directory. Attach one file at a time.\n", path)
	}

	contentType := contentTypeFlag
	if contentType == "" {
		contentType, err = contentTypeFor(path)
		if err != nil {
			return err
		}
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

	// The cap is read from the schema, never from a constant here: it is the server's
	// number, and a copy in this binary would go stale the day it changed and start
	// refusing files the API would take.
	schema, _, _, err := a.schema(ctx, client, false)
	if err != nil {
		return err
	}
	if err := checkSize(info.Size(), schema); err != nil {
		return err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Usagef("Could not read %s: %s\n", path, err)
	}

	document, raw, err := client.AttachDocument(ctx, fieldID, api.Upload{
		Filename:    filepath.Base(path),
		ContentType: contentType,
		Content:     content,
	}, a.uploadTimeout())
	if err != nil {
		return a.classifyWrite(err, "uploading", "dossier fields list")
	}

	if a.JSON {
		a.emitRawPage(raw)
		return nil
	}

	a.renderDocumentBlock(*document)
	a.Notef("\nAttached. The field now reports has_document.\n")
	return nil
}

// checkSize refuses an oversized file locally, quoting the server's own cap.
//
// The point is not to save the server the work; it is that the alternative is uploading
// 26 MiB over a domestic uplink in order to be told the limit is 25. When the schema does
// not publish the cap the upload goes ahead: the server is the authority, and inventing a
// bound from its absence would refuse files that are perfectly fine.
func checkSize(size int64, schema *api.Schema) error {
	max, published := schema.MaxFileBytes()
	if !published || size <= max {
		return nil
	}

	return Usagef(
		"That file is %s. The API's limit is %s per document.\n\n"+
			"Nothing was uploaded.\n",
		humaniseBytes(size), humaniseBytes(max),
	)
}

// extensionTypes maps the extensions the API's allow-list corresponds to.
//
// This is not the allow-list. The CLI cannot hold that — the schema does not publish it
// (the plan's gap 5), and a copy here would be a second source of truth that goes stale
// silently. This is only the extension-to-type guess the declaration is built from, and
// an extension not in it is not refused: --content-type exists precisely so a file whose
// name says nothing can still be sent, and the server decides in either case.
//
// mime.TypeByExtension is deliberately not used. It reads /etc/mime.types, so the type
// declared for the same passport.pdf would depend on which packages happened to be
// installed on the machine — and on a minimal container it would come back empty.
var extensionTypes = map[string]string{
	".pdf":  "application/pdf",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
}

func contentTypeFor(path string) (string, error) {
	extension := strings.ToLower(filepath.Ext(path))
	if contentType, known := extensionTypes[extension]; known {
		return contentType, nil
	}

	if extension == "" {
		return "", Usagef(
			"%s has no extension, so there is nothing to declare a content type from.\n\n"+
				"Pass --content-type, e.g. --content-type application/pdf.\n", path)
	}
	return "", Usagef(
		"Nothing here knows what %s means, so the upload has no type to declare.\n\n"+
			"Pass --content-type if you know what it is. The API's accepted types are in\n"+
			"its reference; it checks the type you declare, not the file's contents.\n",
		extension)
}

// humaniseBytes renders a byte count the way the refusal needs to read.
//
// Both the size and the cap go through it, so "that file is 26.0 MB, the limit is 25.0 MB"
// is a comparison a reader can make at a glance. MiB, because that is what the API's cap
// actually is (25 × 1048576) and rendering it as "26.2 MB" against a file the holder
// knows as 25 MB would invite the wrong conclusion.
func humaniseBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	value, exponent := float64(n)/unit, 0
	for value >= unit && exponent < 2 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"KiB", "MiB", "GiB"}[exponent])
}

// uploadTimeout is the request budget for an attach.
//
// --timeout still wins if it was given, because a holder who asked for a bound meant it.
// Otherwise this is the upload default, not the 30s one: the cap is 25 MiB, and a large
// file on a slow uplink is a slow request rather than a stuck one.
func (a *App) uploadTimeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return api.UploadTimeout
}

func (a *App) renderDocumentBlock(document api.AttachedDocument) {
	block := render.NewBlock(len("CONTENT_TYPE"))
	block.Add("ID", strconv.Itoa(document.ID))
	block.Add("FIELD_ID", strconv.Itoa(document.FieldID))
	block.Add("BYTE_SIZE", strconv.FormatInt(document.ByteSize, 10))
	block.Add("CONTENT_TYPE", document.ContentType)
	_ = block.Render(a.Stdout)
}
