package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PinHeader is where the PIN travels on the document endpoint. On `view` it goes in the
// body instead. Neither is a URL, which is the API's own rule and the reason a PIN never
// reaches a proxy log, a shell history or a `ps` listing.
const PinHeader = "X-Dossier-Pin"

// Dossier is what a recipient receives from POST /api/v1/dossiers/:token/view.
//
// Note what is absent, and deliberately so. There is no `burn_after_read`: the recipient
// boundary publishes no pre-PIN metadata (§12 gap 4) and does not report the flag after
// the fact either, so a client cannot warn that this open is the only one. And there is
// no value on a redacted row — rule 5 means a field the recipient was not given never
// reaches the client at all, in any form, so RedactedField carries a length and nothing
// else.
type Dossier struct {
	Title     string  `json:"title"`
	State     string  `json:"state"`
	ExpiresAt *string `json:"expires_at"`

	Holder         Holder            `json:"holder"`
	ReleasedFields []ReleasedField   `json:"released_fields"`
	RedactedFields []RedactedField   `json:"redacted_fields"`
	Documents      []DossierDocument `json:"documents"`
}

// Holder is the mint-time identity snapshot, frozen at issue.
type Holder struct {
	Name        string `json:"name"`
	Nationality string `json:"nationality"`
}

// ReleasedField is one value the holder chose to release. Country is null for a field
// that is not country-scoped.
type ReleasedField struct {
	Label   string  `json:"label"`
	Value   string  `json:"value"`
	Country *string `json:"country"`
}

// RedactedField is a field the recipient was not given. MaskLength is how wide to draw
// the bar — it is a length, not a value, and is the only thing the server will say.
type RedactedField struct {
	Label      string `json:"label"`
	MaskLength int    `json:"mask_length"`
}

// DossierDocument is a released document. Path is the API's own path for fetching it,
// sent ready-made so a client never has to build one — and, usefully, so the token case
// in it is the server's rather than whatever the recipient typed.
type DossierDocument struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Path        string `json:"path"`
}

// DossierResult is a decoded open plus the bytes it came from, so --json can emit the
// server's own document rather than a re-encoding of this struct.
type DossierResult struct {
	Dossier Dossier
	Raw     []byte
}

// DocumentLink is GET /api/v1/dossiers/:token/documents/:id — a short-lived signed URL
// in the body rather than a redirect, so a client decides whether to follow it.
type DocumentLink struct {
	URL       string `json:"url"`
	ExpiresIn int    `json:"expires_in"`
	Raw       []byte `json:"-"`
}

// OpenDossier sends the one and only POST .../view of an invocation.
//
// The PIN travels in the body. `Once` is set, so this package will not resend it even on
// the 429 it would otherwise wait out: for a burn-after-read dossier the request that
// just failed may have been the open that spent it, and only the holder can say.
func (c *Client) OpenDossier(ctx context.Context, token, pin string, timeout time.Duration) (*DossierResult, error) {
	body, err := json.Marshal(map[string]string{"pin": pin})
	if err != nil {
		return nil, fmt.Errorf("could not build the request: %w", err)
	}

	resp, err := c.Do(ctx, Request{
		Method:    http.MethodPost,
		Path:      Prefix + "/dossiers/" + url.PathEscape(token) + "/view",
		Body:      body,
		Anonymous: true,
		Once:      true,
		Timeout:   timeout,
	})
	if err != nil {
		return nil, err
	}

	var wire struct {
		Dossier Dossier `json:"dossier"`
	}
	if err := resp.Decode(&wire); err != nil {
		return nil, err
	}
	return &DossierResult{Dossier: wire.Dossier, Raw: resp.Body}, nil
}

// DocumentLink fetches the signed URL for one released document. The PIN travels in a
// header here, not the body, because this is a GET.
//
// Not marked Once. Unlike `view` this spends nothing: the API refuses documents on a
// burn-after-read dossier outright, before it even checks the PIN, so there is no open
// to consume. It does write a `doc-view` audit row, which is why the command still does
// not retry it itself — but a rate limit waited out here costs the recipient nothing.
func (c *Client) DocumentLink(ctx context.Context, token string, id int, pin string, timeout time.Duration) (*DocumentLink, error) {
	resp, err := c.Do(ctx, Request{
		Method:    http.MethodGet,
		Path:      Prefix + "/dossiers/" + url.PathEscape(token) + "/documents/" + strconv.Itoa(id),
		Headers:   map[string]string{PinHeader: pin},
		Anonymous: true,
		Timeout:   timeout,
	})
	if err != nil {
		return nil, err
	}

	link := &DocumentLink{}
	if err := resp.Decode(link); err != nil {
		return nil, err
	}
	link.Raw = resp.Body
	return link, nil
}

// FetchSigned downloads the bytes behind a signed URL and streams them to w.
//
// Deliberately not routed through Client.Do, and the reason is the one invariant §8.3
// rests on. Do refuses any path outside /api/v1, which is what makes "the CLI has no web
// path" structural rather than careful — and a signed URL is outside it by construction:
// in production it addresses the Tigris bucket on another host entirely, and in the test
// environment the Disk service answers on /rails/active_storage/... Routing this through
// Do would mean relaxing that check for every caller, to accommodate the one request the
// CLI does not originate.
//
// So the distinction the invariant actually draws is not "every request is under
// /api/v1" but "every request the CLI *composes* is under /api/v1". This one it does not
// compose: it follows a URL the API handed it, once, and treats it as opaque.
//
// No credential of ours is attached — the capability is in the URL, it lasts about five
// minutes, and adding a bearer token to an off-host request would leak it to whoever the
// URL points at.
func FetchSigned(ctx context.Context, client *http.Client, signed string, w io.Writer) (int64, error) {
	parsed, err := url.Parse(signed)
	if err != nil {
		return 0, fmt.Errorf("the API returned a download URL that could not be parsed: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// A file:// or data: URL here would turn a compromised or buggy server into a
		// local-file read. Cheap to refuse, and there is no legitimate case for it.
		return 0, fmt.Errorf("refusing a download URL with scheme %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		return 0, fmt.Errorf("could not build the download request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not fetch the document: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Not an API envelope — this is object storage, which answers in XML or nothing.
		// The likeliest cause by far is the five minutes running out.
		return 0, fmt.Errorf("the download URL returned HTTP %d; signed links last about "+
			"five minutes, so it may simply have expired", resp.StatusCode)
	}

	written, err := io.Copy(w, resp.Body)
	if err != nil {
		return written, fmt.Errorf("the download was interrupted after %d bytes: %w", written, err)
	}
	return written, nil
}
