// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package llm is a Go client for the LF LiteLLM proxy, which exposes an
// OpenAI-compatible /chat/completions surface in front of Bedrock-hosted Claude.
// It is this service's ONLY route to a model. It reads no environment variables
// (Config is injected) and holds no domain vocabulary: prompt composition lives
// with the domain that owns the prompt. Structure otherwise mirrors the sibling
// platform clients: no-follow redirects, bounded reads, per-attempt deadlines,
// body-free typed errors, 429 retry honouring Retry-After.
//
// The reasoning behind each decision below — why 429 retry is safe here when it
// is not for the ad platforms, why errors are rebuilt rather than forwarded, why
// this is non-streaming — is in docs/knowledge/code/internal-platform-llm.md.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultModel is the Bedrock inference profile id the proxy routes on. Carried
	// verbatim from lfx-one: the proxy matches this exact string, so a shorter
	// "modernised" alias would not resolve.
	DefaultModel = "us.anthropic.claude-sonnet-4-20250514-v1:0"

	// DefaultTemperature matches the lfx-one brief pipeline: regenerating after a
	// result the operator disliked should not return the same words.
	DefaultTemperature = 0.7

	// DefaultMaxTokens bounds one completion.
	DefaultMaxTokens = 4096

	// requestTimeout bounds a single HTTP attempt, enforced in do via a context
	// deadline and NOT via http.Client.Timeout — an injected client has none.
	requestTimeout = 30 * time.Second

	// retryMax bounds 429 retries, lower than the siblings' 3 because what is bounded
	// here is token cost, not a risk of double-creating a paid resource.
	retryMax = 2

	retryBaseDelay = 1 * time.Second

	// maxRetryWait caps one backoff; a longer server-declared Retry-After aborts
	// rather than sleeping, so a hostile value cannot wedge the client.
	maxRetryWait = 20 * time.Second

	// maxResponseBody stops a misrouted reply exhausting the pod; a completion capped
	// at DefaultMaxTokens cannot approach it.
	maxResponseBody = 8 << 20 // 8 MiB

	// drainLimit is how much of an unwanted body is read before Close, so net/http
	// returns the connection to the idle pool instead of the retry reopening TCP+TLS.
	drainLimit = 64 << 10
)

var (
	// ErrNotConfigured means the proxy URL or key is missing; callers read it as
	// "this deployment has no model wired" and degrade.
	ErrNotConfigured = errors.New("llm: proxy is not configured")

	// ErrEmptyCompletion means a 200 carried no usable content — distinct from a
	// transport error, since the request did reach the model.
	ErrEmptyCompletion = errors.New("llm: proxy returned no completion content")
)

// Config is the injected proxy configuration. ProxyURL and APIKey are required;
// the rest override the Default* constants above.
type Config struct {
	ProxyURL string
	APIKey   string
	Model    string
	// Temperature is a POINTER because 0 is a meaningful request (deterministic
	// output) that a plain float64 could not distinguish from an unset field.
	Temperature *float64
	MaxTokens   int
}

// Client calls the LiteLLM proxy. Safe for concurrent use. The last three fields
// are injectable so tests avoid real sleeps and can compute Retry-After dates.
type Client struct {
	cfg            Config
	httpClient     *http.Client
	retryBaseDelay time.Duration
	requestTimeout time.Duration
	now            func() time.Time
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient injects the HTTP client. The no-redirect policy is REIMPOSED on
// a copy rather than inherited: refusing redirects is a security property of this
// package, and injection exists to supply a transport, so inheriting the caller's
// policy would make that guard depend on every call site remembering it. The copy
// avoids changing redirect behaviour for other users of that value.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h == nil {
			return
		}
		cp := *h
		cp.CheckRedirect = noFollow
		c.httpClient = &cp
	}
}

// WithRetryBaseDelay injects the retry backoff base.
func WithRetryBaseDelay(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.retryBaseDelay = d
		}
	}
}

// WithRequestTimeout injects the per-attempt timeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.requestTimeout = d
		}
	}
}

// NewClient builds a client from injected config. A missing URL or key fails HERE
// rather than at call time, so a deployment without a model wired is discovered
// once at construction and callers are routed to their degraded path.
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	// Normalize before validating, and STORE the normalized form — mirroring the
	// hubspot/meta/twitter clients. Trimming only for the emptiness check would let a
	// padded value pass construction and fail much later, somewhere less legible: these
	// arrive from Kubernetes secrets, where a trailing newline is the single most common
	// way a value is malformed, and each field carries it differently. A key ending in
	// "\n" builds an Authorization header Go's transport rejects outright as an invalid
	// header value; a proxy URL with surrounding space no longer parses as the URL it
	// looks like. Both then report as a request failure at generation time rather than as
	// the misconfiguration they are, which is exactly what constructing eagerly was meant
	// to avoid. The model id is trimmed on the same grounds — padded, it selects no route
	// on the proxy while looking correct in a log.
	cfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.ProxyURL == "" || cfg.APIKey == "" {
		return nil, ErrNotConfigured
	}
	c := &Client{
		cfg:            cfg,
		httpClient:     &http.Client{CheckRedirect: noFollow},
		retryBaseDelay: retryBaseDelay,
		requestTimeout: requestTimeout,
		now:            time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// noFollow refuses redirects; following one resends the bearer credential.
func noFollow(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// The OpenAI-compatible subset the proxy speaks.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Complete sends one system+user prompt pair and returns the model's text. The
// result is NOT parsed or validated: a caller expecting JSON parses it itself, and
// must enforce its own limits in code rather than trusting the prompt's stated ones.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	temp := DefaultTemperature
	if c.cfg.Temperature != nil {
		temp = *c.cfg.Temperature
	}
	// No TrimSpace here: NewClient is the only writer of cfg and normalizes it, so a
	// whitespace-only model has already become "". Re-trimming would imply the field
	// might arrive padded, which is the invariant this package now holds.
	model := c.cfg.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := c.cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	body, err := json.Marshal(chatRequest{
		Model:       model,
		Temperature: temp,
		MaxTokens:   maxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	raw, err := c.do(ctx, body)
	if err != nil {
		return "", err
	}

	var resp chatResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		// The body is not quoted: a malformed reply may be an upstream error
		// envelope, and those have been observed to echo request context back.
		return "", fmt.Errorf("llm: decode response: %w", uerr)
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return "", ErrEmptyCompletion
	}
	return resp.Choices[0].Message.Content, nil
}

// do performs the POST with per-attempt deadlines and bounded 429 retry.
func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	endpoint := strings.TrimRight(c.cfg.ProxyURL, "/") + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt <= retryMax; attempt++ {
		// Checked BEFORE the attempt, and not inferred from the remaining budget: a
		// context cancelled without a deadline still reports a full budget, so a
		// caller who has gone away would otherwise get one more request made for them.
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("llm: %w", cerr)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
		data, retryAfter, err := c.attempt(attemptCtx, endpoint, body)
		cancel()
		if err == nil {
			return data, nil
		}
		lastErr = err
		if retryAfter < 0 {
			return nil, err // not retryable
		}
		// No attempts left, so there is nothing to wait FOR. Sleeping here would delay a
		// known-final error by the full backoff — up to maxRetryWait when the server sent a
		// large Retry-After — while the caller's own deadline burns down.
		if attempt == retryMax {
			return nil, err
		}
		wait := c.backoff(attempt, retryAfter)
		if wait > maxRetryWait {
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("llm: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// attempt performs one round-trip. retryAfter is negative when the failure is not
// retryable, otherwise the server-declared delay (zero if none was sent).
func (c *Client) attempt(ctx context.Context, endpoint string, body []byte) (data []byte, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, -1, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// redactTransport, never the raw error: the request carries the API key in a
		// header, and a custom RoundTripper can render request context into its message.
		return nil, -1, fmt.Errorf("llm: request failed: %w", redactTransport(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		drain(resp.Body)
		return nil, c.retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("llm: proxy rate limited the request (429)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drain(resp.Body)
		// Status only: an error body from a proxy fronting a model can contain the
		// prompt, which carries operator-supplied campaign context.
		return nil, -1, fmt.Errorf("llm: proxy returned status %d", resp.StatusCode)
	}

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, -1, fmt.Errorf("llm: read response: %w", redactTransport(err))
	}
	return data, -1, nil
}

// backoff is the server's Retry-After when it sent one, else exponential.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return c.retryBaseDelay << attempt
}

// retryAfter parses both documented forms (delta-seconds and an HTTP date).
// Absent or unparseable yields zero: "back off on our own schedule", not "do not retry".
func (c *Client) retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
		return secs
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d
		}
	}
	return 0
}

// drain reads a bounded amount of an unwanted body so the connection can be reused.
// Best effort: a failure here costs a connection, not a request.
func drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, drainLimit))
}

// redactTransport maps a transport error onto values THIS package constructs, so
// nothing from the original chain — which may carry the Authorization header —
// survives into anything that renders or errors.As-walks it. It REBUILDS rather
// than unwraps: peeling *url.Error layers is not enough, since the layer carrying
// untrusted text need not be a *url.Error (a RoundTripper can return
// fmt.Errorf("Bearer <key>: %w", cause), which http.Client.Do then wraps).
// Anything unrecognised defaults to a sentinel.
func redactTransport(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, syscall.ECONNREFUSED):
		return syscall.ECONNREFUSED
	case errors.Is(err, syscall.ECONNRESET):
		return syscall.ECONNRESET
	case errors.Is(err, syscall.ETIMEDOUT):
		return syscall.ETIMEDOUT
	}
	// A DNS error is rebuilt from its BOOLEAN bits alone — callers branch on
	// IsNotFound — so nothing else the resolver attached survives.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &net.DNSError{
			Err:         "dns lookup failed",
			IsNotFound:  dnsErr.IsNotFound,
			IsTimeout:   dnsErr.IsTimeout,
			IsTemporary: dnsErr.IsTemporary,
		}
	}
	return errors.New("llm: transport error contacting the proxy")
}
