package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// IdempotencyHeader is the header POST /api/v1/shares requires. It is the only endpoint in
// the API that reads it.
const IdempotencyHeader = "Idempotency-Key"

// IdempotencyKeyMaxBytes is the API's documented ceiling. Over it is a validation failure
// rather than the missing-key error, because the key was present — just too long.
const IdempotencyKeyMaxBytes = 255

// MintInput is what a caller wants to release. It is turned into bytes exactly once, by
// BuildMintBody, and those bytes are what every attempt sends.
type MintInput struct {
	Title      string
	FieldIDs   []int
	Recipients []Recipient

	// DocumentIDs distinguishes three states the API treats differently: omitted means
	// every document on every released field rides along, an explicit empty list strips
	// them all, and a list names the ones to attach. A plain []int cannot express that —
	// nil and []int{} both marshal to the same thing under omitempty and to the wrong one
	// without it — so the pointer is load-bearing.
	DocumentIDs *[]int

	// ExpiresAt nil is the JSON null the API requires for the labelled no-expiry
	// exception. The key is always sent; see mintBody.
	ExpiresAt *time.Time

	// The three booleans are pointers so an unset flag omits the key and leaves the
	// column's own default, which is what docs/api-shares.md asks callers to do. Sending
	// `false` explicitly is a different request from not mentioning it.
	BurnAfterRead         *bool
	AllowDocumentDownload *bool
	NotifyOnOpen          *bool
}

// mintBody is the wire shape. Separate from MintInput so the JSON tags — and in
// particular which keys may be omitted — are stated in one place and cannot be changed by
// editing the caller-facing struct.
type mintBody struct {
	Title       *string     `json:"title,omitempty"`
	FieldIDs    []int       `json:"field_ids"`
	DocumentIDs *[]int      `json:"document_ids,omitempty"`
	Recipients  []Recipient `json:"recipients,omitempty"`

	// No omitempty, deliberately: the API requires this key to be present and answers
	// `422 expiry_required` when it is absent. A nil pointer marshals to `null`, which is
	// the only spelling of the no-expiry exception the API accepts — an empty string or a
	// false would both be a 422.
	ExpiresAt *string `json:"expires_at"`

	BurnAfterRead         *bool `json:"burn_after_read,omitempty"`
	AllowDocumentDownload *bool `json:"allow_document_download,omitempty"`
	NotifyOnOpen          *bool `json:"notify_on_open,omitempty"`
}

// BuildMintBody serialises a mint into the bytes that will be sent, once.
//
// The plan's §6.2 makes this a separate step from sending for one reason: the API's
// idempotency identity is the key plus the *raw body*, so a retry has to send
// byte-identical bytes or earn `409 idempotency_key_reused`. Marshalling per attempt would
// rely on encoding/json being stable for this struct, which it is — but relying on it is
// the kind of assumption that holds until someone adds a map. Building the slice once and
// storing it in the ledger makes the client's digest and the server's agree by
// construction rather than by luck.
//
// field_ids are sent in the order given, unsorted and undeduplicated. Either would be a
// different body, and a client that quietly rewrote the request would break the one
// property this whole mechanism exists to provide.
func BuildMintBody(input MintInput) ([]byte, error) {
	body := mintBody{
		FieldIDs:              input.FieldIDs,
		DocumentIDs:           input.DocumentIDs,
		Recipients:            input.Recipients,
		BurnAfterRead:         input.BurnAfterRead,
		AllowDocumentDownload: input.AllowDocumentDownload,
		NotifyOnOpen:          input.NotifyOnOpen,
	}
	if input.Title != "" {
		body.Title = &input.Title
	}
	if input.ExpiresAt != nil {
		formatted := input.ExpiresAt.UTC().Format(time.RFC3339)
		body.ExpiresAt = &formatted
	}
	if body.FieldIDs == nil {
		// An explicit empty array rather than null. The API reads `[]` as "no ids given"
		// and answers `no_fields`, which is the honest outcome; `null` is a shape its
		// parameter handling does not document.
		body.FieldIDs = []int{}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("could not encode the mint: %w", err)
	}
	return raw, nil
}

// BodyDigest is the SHA-256 of the bytes, hex encoded.
//
// Stored in the ledger beside the body so a resume can prove the bytes it is about to
// resend are the bytes the key was first used with. The server computes its own digest of
// the raw body; this is not that digest and does not need to match it — it exists to catch
// a ledger file that was edited or truncated, before the CLI sends something under a key
// that would then be permanently poisoned.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// MintedShare is one entry of the 201's shares array.
//
// This is the only response in the API that carries a raw PIN, and the only place the
// CLI ever sees one. It is not stored anywhere: not in the ledger, not in the schema
// cache, not in a profile.
type MintedShare struct {
	ID    int    `json:"id"`
	Token string `json:"token"`
	URL   string `json:"url"`

	PIN          string `json:"pin"`
	PINShownOnce bool   `json:"pin_shown_once"`

	Recipient  Recipient  `json:"recipient"`
	State      string     `json:"state"`
	ExpiresAt  *time.Time `json:"expires_at"`
	FieldCount int        `json:"field_count"`

	// EmailQueued is exactly `recipient.email` being present. It says an address was on
	// file, not that a message arrived — the API has no way to tell a caller the latter,
	// and the renderer must not imply it does.
	EmailQueued bool `json:"email_queued"`
}

// MintResult is the 201 body.
type MintResult struct {
	BatchID string        `json:"batch_id"`
	Shares  []MintedShare `json:"shares"`

	// Raw is the body byte-for-byte, for --json. Under §7.4 that passes the PIN through
	// too, which is the holder's decision when they ask for --json; the CLI does not
	// redact a response the API chose to return.
	Raw []byte `json:"-"`
}

// Mint sends one mint attempt under one key.
//
// Takes pre-built bytes rather than a MintInput, so that a retry and a --resume are
// literally the same call with the same slice. Every retry decision belongs to the caller:
// this returns what happened and nothing more, because the rules in the plan's §6.3 need
// the ledger's memory of previous attempts — whether a 5xx was already seen under this key
// is what tells a `409 request_in_flight` apart from a claim that will never resolve, and
// a transport has no business knowing that.
func (c *Client) Mint(ctx context.Context, key string, body []byte, timeout time.Duration) (*MintResult, error) {
	response, err := c.Do(ctx, Request{
		Method:      http.MethodPost,
		Path:        Prefix + "/shares",
		Body:        body,
		ContentType: "application/json",
		Headers:     map[string]string{IdempotencyHeader: key},
		Timeout:     timeout,
	})
	if err != nil {
		return nil, err
	}

	var result MintResult
	if err := response.Decode(&result); err != nil {
		return nil, err
	}
	result.Raw = response.Body
	return &result, nil
}
