package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prefix is the only path prefix this client will send to.
//
// The plan's §1 makes this a hard restriction rather than a convention: the CLI speaks to
// /api/v1 and nothing else — never a web route, never a cookie, never scraped HTML. That
// single rule is what keeps every property the API reference proves (no value to the
// owner, no un-burned burn share, no credential management) true of the client by
// construction instead of by care. Enforcing it here, at the one place requests are
// built, is what makes it structural.
const Prefix = "/api/v1"

// DefaultTimeout is the per-request budget for everything except uploads.
const DefaultTimeout = 30 * time.Second

// DefaultWaitBudget caps the total time the client will spend asleep honouring
// Retry-After across one logical call. The server decides each individual wait; this
// decides how long the CLI is willing to keep being patient before reporting TryLater and
// letting the caller decide.
const DefaultWaitBudget = 60 * time.Second

// Client is an authenticated (or deliberately anonymous) transport for one host.
type Client struct {
	host  string
	token string

	http        *http.Client
	userAgent   string
	waitBudget  time.Duration
	noWait      bool
	maxAttempts int

	// Injected so the retry logic is testable without real time passing. Production wires
	// these to time.Now and time.Sleep.
	now   func() time.Time
	sleep func(time.Duration)
}

// Option configures a Client.
type Option func(*Client)

// WithToken sets the bearer token sent on every non-anonymous request.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithUserAgent sets the User-Agent. Carries the version so a server-side log can tell
// which binary sent a request.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithNoWait makes the client refuse to sleep on Retry-After, failing immediately
// instead. This is --no-wait: a cron job with a five-minute window would rather fail now
// and be retried by its scheduler than block.
func WithNoWait(noWait bool) Option {
	return func(c *Client) { c.noWait = noWait }
}

// WithWaitBudget caps total sleep across one call.
func WithWaitBudget(d time.Duration) Option {
	return func(c *Client) { c.waitBudget = d }
}

// WithClock injects time, for tests.
func WithClock(now func() time.Time, sleep func(time.Duration)) Option {
	return func(c *Client) {
		c.now = now
		c.sleep = sleep
	}
}

// New builds a client for a host. The host is the origin only
// (https://dossier.global); paths are supplied per request and must carry Prefix.
func New(host string, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSuffix(host, "/"))
	if err != nil {
		return nil, fmt.Errorf("host %q is not a URL: %w", host, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("host %q must be http or https", host)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("host %q has no hostname", host)
	}

	c := &Client{
		host:        parsed.String(),
		userAgent:   "dossier-cli",
		waitBudget:  DefaultWaitBudget,
		maxAttempts: 4,
		now:         time.Now,
		sleep:       time.Sleep,
		http: &http.Client{
			// No cookie jar, ever. The CLI is not a browser session and must never
			// accumulate one: a cookie is how a client accidentally starts acting as the
			// signed-in holder rather than as a bearer token, which is the boundary the
			// whole API design rests on.
			Jar: nil,

			// Redirects are refused rather than followed. The API does not redirect, so a
			// redirect means something is in front of it — a proxy, a captive portal, a
			// misconfigured host — and following one with an Authorization header
			// attached is how a bearer token ends up somewhere it was never minted for.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("refusing to follow a redirect to %s: the Dossier API does not redirect", req.URL.Redacted())
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Host returns the origin this client talks to.
func (c *Client) Host() string { return c.host }

// Request is one call. Body is bytes rather than an `any` to be marshalled, deliberately:
// the plan's §6.2 requires that a retry send *identical* bytes, because the API's
// idempotency identity is the key plus the raw body. A struct re-serialised per attempt
// can differ in map ordering and would be a different request wearing the same key.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte

	ContentType string
	Headers     map[string]string

	// Anonymous omits the Authorization header. Only GET /api/v1/schema uses it: a
	// discovery document a client cannot read without already holding a credential
	// teaches it nothing, so that endpoint takes no token — and sending one anyway would
	// spend a unit of the failed-auth budget if the stored token happened to be dead.
	Anonymous bool

	Timeout time.Duration
}

// Response is a successful (2xx) reply. Failures come back as *Error.
type Response struct {
	Status int
	Body   []byte
	Header http.Header

	// Attempts is how many round trips this call took, including the successful one.
	// Surfaced so a caller can tell the holder it waited, rather than appearing to hang.
	Attempts int
}

// Decode unmarshals the body into v.
func (r *Response) Decode(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("the response was not the JSON this command expected: %w", err)
	}
	return nil
}

// Do sends a request, honouring Retry-After on 429 within the wait budget, and returns
// either a 2xx Response or an *Error.
//
// What it deliberately does not retry: anything other than 429. The API's reference is
// explicit about which failures leave no trace and which may have, and only the
// rate limiter is unambiguously safe to repeat blind. A 5xx on mint *is* retried once,
// but that decision belongs to the mint command, which owns the idempotency key and the
// ledger — not here, where a generic retry would silently apply it to endpoints that have
// no idempotency guard at all.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if !strings.HasPrefix(req.Path, Prefix) {
		// A CLI bug, not a user error: some command built a path outside /api/v1. Caught
		// before any socket opens so the invariant cannot be violated even transiently.
		return nil, fmt.Errorf("refusing to request %q: every path must begin with %s", req.Path, Prefix)
	}

	var slept time.Duration
	for attempt := 1; ; attempt++ {
		resp, apiErr, err := c.attempt(ctx, req)
		if err != nil {
			return nil, err
		}
		if apiErr == nil {
			resp.Attempts = attempt
			return resp, nil
		}

		retryable := apiErr.Code == "rate_limited" && attempt < c.maxAttempts
		if !retryable {
			return nil, apiErr
		}
		if c.noWait {
			return nil, apiErr
		}

		wait := time.Duration(apiErr.RetryAfter) * time.Second
		if wait <= 0 {
			// The server rate-limited us without saying for how long. One second is the
			// floor rather than zero so a retry loop cannot become a hot loop, and it is
			// not an invented backoff curve: if the server wanted a specific wait it
			// would have sent one.
			wait = time.Second
		}
		if slept+wait > c.waitBudget {
			return nil, apiErr
		}

		slept += wait
		c.sleep(wait)
	}
}

// attempt performs exactly one round trip. A nil error with a nil *Error means 2xx.
func (c *Client) attempt(ctx context.Context, req Request) (*Response, *Error, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := c.host + req.Path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}

	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("could not build the request: %w", err)
	}

	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	} else if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if !req.Anonymous && c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		// Transport failures stay transport failures. The command layer turns this into
		// exit 1 and says what it could not reach; it must not be dressed up as an API
		// error, because "the server refused you" and "the server was never reached" lead
		// to different actions.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%s %s timed out after %s", req.Method, req.Path, timeout)
		}
		return nil, nil, fmt.Errorf("could not reach %s: %w", c.host, err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("could not read the response from %s: %w", c.host, err)
	}

	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return &Response{Status: httpResp.StatusCode, Body: raw, Header: httpResp.Header}, nil, nil
	}

	apiErr := &Error{
		Status:     httpResp.StatusCode,
		Body:       raw,
		RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After"), c.now),
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil {
		apiErr.Code = env.Error.Code
		apiErr.Message = env.Error.Message
		apiErr.Hint = env.Error.Hint
	}
	// A non-envelope body on a failure status is left with an empty Code, which maps to
	// exit 1 — "unparseable response". That is the honest outcome: something answered, and
	// it was not this API.
	return nil, apiErr, nil
}

// parseRetryAfter reads the header in both forms RFC 9110 allows: delay-seconds, and an
// HTTP-date. Rails sends seconds, but a proxy in front of it may rewrite the header, and
// a client that silently read 0 from a date would hammer a limiter it was asked to back
// off from.
func parseRetryAfter(value string, now func() time.Time) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		delta := int(when.Sub(now()).Seconds())
		if delta < 0 {
			return 0
		}
		return delta
	}
	return 0
}
