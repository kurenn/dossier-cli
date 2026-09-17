package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// UploadTimeout is the per-request budget for a document upload.
//
// Longer than DefaultTimeout because the cap is 25 MiB and the request is not finished
// until the last byte is sent: a large file over a slow uplink is a slow request, not a
// stuck one, and timing it out at thirty seconds would abandon an upload the server is
// still happily receiving — which, on an endpoint with no idempotency guard, is the
// worst outcome available. See AttachDocument on why that is never retried.
const UploadTimeout = 5 * time.Minute

// CreateFieldInput is the body of POST /api/v1/fields.
//
// Category, Country and Sensitivity are omitted when unset so the server applies its own
// defaults rather than the client asserting them — the same reason no vocabulary is
// hardcoded here. Sensitivity is a pointer for that to work: it has a meaningful zero in
// Go and none in the API, so `omitempty` on a plain int would silently drop a deliberate
// 0 and let the server default to 1 while the holder believed otherwise. A pointer sends
// exactly what was asked for, including a value the server will reject.
type CreateFieldInput struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Category    string `json:"category,omitempty"`
	Country     string `json:"country,omitempty"`
	Sensitivity *int   `json:"sensitivity,omitempty"`
}

// CreateField creates one custom field and returns the metadata row the API echoes.
//
// The response does not contain the value that was just sent, and Field has nowhere to
// put one if it did (see its own comment). So the value exists in this process only as
// the request body, and in the response not at all.
//
// Not retried on anything but 429, which Client.Do handles: this endpoint takes no
// Idempotency-Key (docs/api-fields.md), so a resend after an ambiguous failure is a
// genuinely new create attempt, not a repeat of the old one.
// The raw response body is returned alongside the decoded row, for --json: §7.4 requires
// the bytes the server sent, not this struct re-marshalled into a second shape.
func (c *Client) CreateField(ctx context.Context, in CreateFieldInput, timeout time.Duration) (*Field, []byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, nil, fmt.Errorf("could not encode the field: %w", err)
	}

	response, err := c.Do(ctx, Request{
		Method:  http.MethodPost,
		Path:    Prefix + "/fields",
		Body:    body,
		Timeout: timeout,
	})
	if err != nil {
		return nil, nil, err
	}

	var field Field
	if err := response.Decode(&field); err != nil {
		return nil, nil, err
	}
	return &field, response.Body, nil
}

// Upload is one file for POST /api/v1/fields/:id/documents.
type Upload struct {
	// Filename is the base name sent in the part's Content-Disposition. The API stores
	// it and shows it back in a field's documents array; it is not a path, and no
	// directory component is ever sent.
	Filename string

	// ContentType is the *declared* type of the part. The server allow-lists on this
	// declaration rather than sniffing the bytes, so it is load-bearing: declaring
	// application/octet-stream — which is what multipart.CreateFormFile would do — is a
	// guaranteed 415 on a file the API would otherwise have accepted.
	ContentType string

	Content []byte
}

// AttachedDocument is the body of POST /api/v1/fields/:id/documents, unwrapped from its
// "document" envelope.
//
// Deliberately not the same type as Document, the nested row in a field listing, because
// they are not the same shape: this one names the field it landed on and has no
// filename; that one has the filename and no field id. One struct covering both would
// have two fields that are empty half the time and no way to tell "absent" from "zero".
type AttachedDocument struct {
	ID          int    `json:"id"`
	FieldID     int    `json:"field_id"`
	ByteSize    int64  `json:"byte_size"`
	ContentType string `json:"content_type"`
}

// AttachDocument uploads one file to one field.
//
// Never retried after the body is sent, and not only because Client.Do declines to: this
// endpoint has no idempotency guard *and* is purely additive, so a retry after a timeout
// does not fail and does not deduplicate — it silently attaches a second copy. That is
// the one failure mode in this API where retrying is worse than giving up, because the
// damage is invisible until someone lists the field. The command layer says so in prose
// when a send fails ambiguously.
func (c *Client) AttachDocument(ctx context.Context, fieldID int, upload Upload, timeout time.Duration) (*AttachedDocument, []byte, error) {
	body, contentType, err := multipartBody(upload)
	if err != nil {
		return nil, nil, err
	}

	response, err := c.Do(ctx, Request{
		Method:      http.MethodPost,
		Path:        Prefix + "/fields/" + strconv.Itoa(fieldID) + "/documents",
		Body:        body,
		ContentType: contentType,
		Timeout:     timeout,
	})
	if err != nil {
		return nil, nil, err
	}

	var envelope struct {
		Document AttachedDocument `json:"document"`
	}
	if err := response.Decode(&envelope); err != nil {
		return nil, nil, err
	}
	return &envelope.Document, response.Body, nil
}

// multipartBody builds the one-part form the API expects.
//
// Built by hand rather than with CreateFormFile so the part can declare its real content
// type; CreateFormFile hardcodes application/octet-stream, which this API rejects.
func multipartBody(upload Upload) (body []byte, contentType string, err error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(upload.Filename)))
	header.Set("Content-Type", upload.ContentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("could not build the upload: %w", err)
	}
	if _, err := part.Write(upload.Content); err != nil {
		return nil, "", fmt.Errorf("could not build the upload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("could not build the upload: %w", err)
	}

	return buffer.Bytes(), writer.FormDataContentType(), nil
}

// escapeQuotes mirrors mime/multipart's own unexported escaper, which is what the
// stdlib applies to the filenames it writes. Reimplemented rather than skipped because a
// filename containing a quote would otherwise end the quoted-string early and change
// which part the server thinks it is reading.
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }
