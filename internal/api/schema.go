package api

import (
	"context"
	"net/http"
)

// Schema is GET /api/v1/schema — the discovery document.
//
// This is the CLI's only source for the API's numbers and vocabularies. The plan's §2
// makes that a rule with no exceptions: hardcoded scopes, limits or filter values would
// drift from the server, and the schema is built from the same constants the server
// enforces, so it cannot. The two filter vocabularies below were added to the API
// specifically so this client would never carry a literal list of either.
//
// Fields are a superset-tolerant decode: unknown keys are ignored, so a server that grows
// the document does not break an older binary. Missing keys are the interesting case and
// each command checks the one it needs rather than validating the whole document up front
// — a `schema` command should be able to print a partial document and say what is absent.
type Schema struct {
	Endpoints []Endpoint `json:"endpoints"`
	Scopes    []string   `json:"scopes"`

	FieldCategories []string `json:"field_categories"`
	FieldStatuses   []string `json:"field_statuses"`
	ShareStates     []string `json:"share_states"`

	ErrorCodes []ErrorCode    `json:"error_codes"`
	Limits     map[string]any `json:"limits"`
	Expiry     Expiry         `json:"expiry"`

	// Raw is the document byte-for-byte, kept so --json passes through what the server
	// sent rather than this struct re-serialised. §7.4: the API's shape is the contract,
	// and re-marshalling would invent a second one.
	Raw []byte `json:"-"`
}

// Endpoint is one row of the endpoint list.
type Endpoint struct {
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Scope         string   `json:"scope"`
	RequestParams []string `json:"request_params"`
	ResponseKeys  []string `json:"response_keys"`
}

// ErrorCode is one row of the error table.
type ErrorCode struct {
	Code    string `json:"code"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// Expiry carries the mint expiry rules, including the labelled no-expiry exception.
//
// The shape matters to the CLI's picker: rule 2 says expiry is required, not a setting,
// and that "no expiry" exists but is labelled as the exception. The API models that by
// putting the presets in a list and the exception in its own object with its own label
// and note — so a client that renders them as equal options is contradicting the product,
// and one that renders this faithfully cannot.
type Expiry struct {
	Required bool     `json:"required"`
	Presets  []string `json:"presets"`
	Note     string   `json:"note"`
	NoExpiry struct {
		Value any    `json:"value"`
		Label string `json:"label"`
		Note  string `json:"note"`
	} `json:"no_expiry"`
}

// ErrorCodeStrings returns just the codes, for exitcode.Unmapped.
func (s *Schema) ErrorCodeStrings() []string {
	codes := make([]string, 0, len(s.ErrorCodes))
	for _, row := range s.ErrorCodes {
		codes = append(codes, row.Code)
	}
	return codes
}

// FetchSchema gets the discovery document.
//
// Sent anonymously even when a token is stored. The endpoint takes no credential, and
// sending a dead one would spend a unit of the per-IP failed-auth budget for no benefit —
// which would make `dossier schema`, the one command that works before any token exists,
// fail for a reason that has nothing to do with it.
func (c *Client) FetchSchema(ctx context.Context) (*Schema, error) {
	resp, err := c.Do(ctx, Request{
		Method:    http.MethodGet,
		Path:      Prefix + "/schema",
		Anonymous: true,
	})
	if err != nil {
		return nil, err
	}

	var schema Schema
	if err := resp.Decode(&schema); err != nil {
		return nil, err
	}
	schema.Raw = resp.Body
	return &schema, nil
}
