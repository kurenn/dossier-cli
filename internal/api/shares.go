package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Share is one row of GET /api/v1/shares, and the `share` object of GET
// /api/v1/shares/:id — the same nine-and-one keys from the same builder, so one type.
//
// The PIN is absent and unrepresentable, as it is in the API: `pin_digest` is bcrypt and
// no read path could reconstruct it. Only the mint response ever carries one.
type Share struct {
	ID        int       `json:"id"`
	Token     string    `json:"token"`
	Title     *string   `json:"title"`
	Recipient Recipient `json:"recipient"`

	// State is rendered, never computed (product rule 2, and the CLI's own
	// non-negotiable 2). Kept as a string rather than a typed enum so that a state this
	// binary has not heard of still round-trips to the screen instead of becoming a zero
	// value.
	State string `json:"state"`

	// ExpiresAt and RevokedAt are the whole deadline story and neither answers it alone.
	// Revoking does not touch the expiry, so a revoked share keeps whatever future
	// ExpiresAt it was minted with — render.Deadline is where that trap is handled.
	//
	// RevokedAt arrived later than the rest of this object (gap 14); a server without it
	// leaves this nil, and the renderer degrades to saying the share is closed without
	// saying when.
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`

	// RevokedReason says which of the three ways it closed: "holder_revoked",
	// "burned_after_read" or "kill_switch". Empty while the share is open, so it is read
	// with RevokedAt rather than instead of it.
	//
	// A string, not a typed enum, for the same reason as State: a reason this binary has
	// not heard of should reach the screen, not become a zero value.
	RevokedReason string `json:"revoked_reason"`

	// BurnAfterRead and AllowDocumentDownload are the terms the share was released on.
	// Both were write-only until the API grew them (gap 15) — settable at mint and
	// readable nowhere — so a server without them leaves these false, which reads as the
	// safer of the two answers for BurnAfterRead and the more restrictive for
	// AllowDocumentDownload.
	//
	// BurnAfterRead is the one that matters: it is the difference between a dossier that
	// can be opened and one that is spent by opening it, and it is only useful *before*
	// the open. Afterwards the share is already revoked and RevokedReason tells the story.
	BurnAfterRead         bool `json:"burn_after_read"`
	AllowDocumentDownload bool `json:"allow_document_download"`

	OpensCount int `json:"opens_count"`
	FieldCount int `json:"field_count"`

	// BatchID groups the sibling shares one mint produced, one per recipient. A
	// screen-only grouping key, not a shared secret.
	BatchID *string `json:"batch_id"`
}

// Recipient is a share's primary recipient. Both halves are optional: the API drops a
// recipient row carrying neither, so a share can have an empty one.
type Recipient struct {
	Label string `json:"label"`
	Email string `json:"email"`
}

// Display is the recipient as one cell — the label, falling back to the email.
//
// The label is preferred because it is what the holder typed and what every other surface
// shows. Empty when there is neither, which the renderer turns into the em dash.
func (r Recipient) Display() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Email
}

// AuditEvent is one row of a share's trail.
//
// Product rule 9: every event carries a name *and* a sentence saying what it means, and
// both come from the server's own 13-key vocabulary — never re-described here. A CLI that
// wrote its own sentence for "pin-fail" would be the second source of truth the rule
// exists to prevent.
type AuditEvent struct {
	Kind       string    `json:"kind"`
	Label      string    `json:"label"`
	Meaning    string    `json:"meaning"`
	OccurredAt time.Time `json:"occurred_at"`
	Detail     *string   `json:"detail"`
}

// SharePage is one page of the share list.
type SharePage struct {
	Shares    []Share `json:"shares"`
	NextAfter *int    `json:"next_after"`
	Raw       []byte  `json:"-"`
}

// ShareDetail is GET /api/v1/shares/:id.
type ShareDetail struct {
	Share       Share        `json:"share"`
	AuditEvents []AuditEvent `json:"audit_events"`
	Raw         []byte       `json:"-"`
}

// ShareQuery is the request parameters for a share listing.
type ShareQuery struct {
	// State is validated by the caller against the schema's share_states, never against a
	// list in this binary.
	State string
	Limit int
	After *int
}

func (q ShareQuery) values() url.Values {
	values := url.Values{}
	if q.State != "" {
		values.Set("state", q.State)
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.After != nil {
		values.Set("after", strconv.Itoa(*q.After))
	}
	return values
}

// ListShares fetches one page of shares, newest id first.
func (c *Client) ListShares(ctx context.Context, query ShareQuery, timeout time.Duration) (*SharePage, error) {
	response, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		Path:    Prefix + "/shares",
		Query:   query.values(),
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}

	var page SharePage
	if err := response.Decode(&page); err != nil {
		return nil, err
	}
	page.Raw = response.Body
	return &page, nil
}

// ShowShare fetches one share and its audit trail.
func (c *Client) ShowShare(ctx context.Context, id int, timeout time.Duration) (*ShareDetail, error) {
	response, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		Path:    Prefix + "/shares/" + strconv.Itoa(id),
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}

	var detail ShareDetail
	if err := response.Decode(&detail); err != nil {
		return nil, err
	}
	detail.Raw = response.Body
	return &detail, nil
}
