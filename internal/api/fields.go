package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Field is one row of GET /api/v1/fields.
//
// Note what is absent and must stay absent: there is no value, and no mask of one. The API
// returns neither (product rule 5 holds at the server), and the point of having no field
// for it here is that no amount of later renderer work can put one on screen. A
// never-leak test asserts the same thing from the outside.
//
// Pointers where the API sends null, so "absent" and "empty string" stay distinguishable:
// a field with no country is not a field whose country is "".
type Field struct {
	ID          int        `json:"id"`
	Label       string     `json:"label"`
	Category    string     `json:"category"`
	Country     *string    `json:"country"`
	Sensitivity int        `json:"sensitivity"`
	Status      string     `json:"status"`
	HasDocument bool       `json:"has_document"`
	Documents   []Document `json:"documents"`
}

// Document is one entry of a field's nested documents array. Rendered as a count in the
// human table (§7.3, a nested array has no column shape); these fields are reachable
// through --json.
type Document struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
}

// FieldPage is one page of the field list.
type FieldPage struct {
	Fields []Field `json:"fields"`

	// NextAfter is the last row's id when a further page exists, null otherwise. A
	// pointer because null is the meaningful value — it is the only thing that says "that
	// was all of them".
	NextAfter *int `json:"next_after"`

	// Raw is the page byte-for-byte, for --json (§7.4). The API's shape is the contract;
	// re-marshalling this struct would invent a second one.
	Raw []byte `json:"-"`
}

// FieldQuery is the request parameters for a field listing.
type FieldQuery struct {
	// Status is validated by the caller against the schema's field_statuses, never
	// against a list in this binary.
	Status string
	Limit  int
	After  *int
}

func (q FieldQuery) values() url.Values {
	values := url.Values{}
	if q.Status != "" {
		values.Set("status", q.Status)
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.After != nil {
		values.Set("after", strconv.Itoa(*q.After))
	}
	return values
}

// ListFields fetches one page of fields.
func (c *Client) ListFields(ctx context.Context, query FieldQuery, timeout time.Duration) (*FieldPage, error) {
	response, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		Path:    Prefix + "/fields",
		Query:   query.values(),
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}

	var page FieldPage
	if err := response.Decode(&page); err != nil {
		return nil, err
	}
	page.Raw = response.Body
	return &page, nil
}
