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
	"net/url"
	"strconv"
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

	// ErrInvalidProxyURL means AI_PROXY_URL is present but is not an absolute
	// http(s) URL. It WRAPS ErrNotConfigured deliberately: to every caller the
	// operational fact is the same — this deployment has no usable model, degrade —
	// and making it a separate sentinel would silently stop each of them degrading.
	// It exists only so the message names the defect instead of saying "missing".
	ErrInvalidProxyURL = fmt.Errorf("%w: proxy url must be an absolute http(s) url", ErrNotConfigured)

	// ErrProxyURLNotABase means the proxy URL carries userinfo, a query or a fragment —
	// components a BASE url has no use for and that this client must refuse rather than
	// tolerate. It wraps ErrInvalidProxyURL, so callers still degrade.
	//
	// Rejecting is the correct behaviour on two independent grounds, and either alone
	// would justify it. The first is mechanical: the endpoint is built by appending
	// "/chat/completions" to this value, and appending a path to a URL that already ended
	// in "?x=1" or "#frag" produces a string whose path is not the one anybody wrote.
	// That value would be accepted at startup and fail — or worse, silently mis-route —
	// on every generation, which is the exact class of late failure this constructor
	// exists to eliminate. The second is disclosure: userinfo and query are where a
	// credential rides inside a URL, and a rejected value can never be logged, stored, or
	// echoed by anything downstream. Tolerating them and stripping at print time would
	// leave the raw value live in Config, in flight, and in whatever else formats it.
	ErrProxyURLNotABase = fmt.Errorf("%w: proxy url must be a base url with no userinfo, query or fragment", ErrInvalidProxyURL)

	// ErrEmptyCompletion means a 200 carried no usable content — distinct from a
	// transport error, since the request did reach the model.
	ErrEmptyCompletion = errors.New("llm: proxy returned no completion content")

	// ErrIncompleteCompletion means a 200 carried content the model did not FINISH
	// producing — most often because max_tokens truncated it mid-sentence. It is a
	// separate sentinel from ErrEmptyCompletion because the failure is the opposite
	// shape: there is real, plausible-looking output, which is precisely why returning
	// it with a nil error is the dangerous answer. Half an email reads like an email.
	ErrIncompleteCompletion = errors.New("llm: proxy returned an incomplete completion")
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
	cfg Config
	// endpoint is the full /chat/completions URL, built and validated once in
	// NewClient. Holding the built form is what lets construction reject a proxy URL
	// that only LOOKS like one — see the validation there.
	endpoint       string
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
// once at construction. The contract for the part-2 consumer that will wire this
// into HubSpot dispatch is that ErrNotConfigured is a DEGRADE signal, not a fatal
// one: copy generation is an enrichment, so the composition root logs it and
// proceeds without a client. No such consumer exists in this PR.
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
	// Temperature is COPIED, not aliased. Every other field of Config is a value, so
	// storing cfg wholesale would otherwise leave one field pointing at memory the
	// caller still owns: Client documents itself safe for concurrent use, and a caller
	// that reuses or mutates its Config after construction would race Complete's read
	// of *c.cfg.Temperature. Snapshotting here makes the whole stored config immutable,
	// which is what that concurrency claim actually requires.
	if cfg.Temperature != nil {
		t := *cfg.Temperature
		cfg.Temperature = &t
	}
	if cfg.ProxyURL == "" || cfg.APIKey == "" {
		return nil, ErrNotConfigured
	}
	// Non-empty is not the same as usable, and the difference is not cosmetic. A value
	// like "localhost:4000" parses happily — url.Parse reads "localhost" as the SCHEME —
	// and http.NewRequest then rejects it on every single generation, so a one-line
	// deployment mistake becomes a per-email failure with a transport-shaped message.
	// This constructor's whole reason for existing is to move that discovery to startup,
	// so it must check what the request will actually need: an absolute URL, an http(s)
	// scheme, and a host.
	//
	// The host check reads Hostname(), not Host. A port-only authority like
	// "http://:4000" parses with Host == ":4000" — non-empty — and http.NewRequest
	// accepts it, so a Host check would pass it through and the failure would surface
	// once per generation at Do() time, which is exactly the outcome this constructor
	// exists to prevent. Hostname() strips the port and is empty for that value. The
	// LinkedIn and Reddit clients guard the same way, for the same reason.
	//
	// No branch below echoes cfg.ProxyURL. A value this constructor is REJECTING is the
	// one least entitled to be quoted: it is unvalidated operator input, and the reasons
	// it can be invalid include "it has a token in its userinfo". Quoting it would put
	// that token in the startup log and in every error string the failure propagates
	// through, which is a worse outcome than the misconfiguration itself. Each branch
	// instead names the COMPONENT it judged and quotes NOTHING.
	//
	// An earlier version of this comment claimed scheme and host were safe to quote,
	// because url.Parse has already split userinfo and query into their own fields. That
	// reasoning does not hold, and the parse-failure branch below is the proof of the
	// general shape: url.Parse does not validate what a component CONTAINS, only where
	// the delimiters fall. A pasted "sk-secret:tok@litellm.internal" parses as an OPAQUE
	// url whose scheme is "sk-secret" — the credential, sitting in the field this switch
	// was quoting. There is no component of a value we are rejecting that we know enough
	// about to reproduce, so none is reproduced.
	u, perr := url.Parse(cfg.ProxyURL)
	switch {
	case perr != nil:
		// The cause is DISCARDED, not unwrapped. url.Error.Error() embeds the raw url,
		// which is the obvious leak — but unwrapping to uerr.Err is only most of the fix,
		// because net/url's causes quote the FRAGMENT of the input they choked on:
		// "https://litellm.internal:sk-secret" yields `invalid port ":sk-secret" after
		// host`, and "%zz" yields `invalid URL escape "%zz"`. A message that reproduces
		// any part of an unvalidated value cannot honour this constructor's no-echo rule,
		// so nothing derived from the input is carried. Nothing is lost that a caller
		// could use: url.Parse's causes are unexported or plain strings, so errors.Is/As
		// reach nothing, and the sentinel is what callers match on. The operator learns
		// which variable is malformed, which is what they need to go and look at it.
		return nil, fmt.Errorf("%w (it does not parse as a url; the value is not quoted "+
			"here because it is unvalidated and may carry a credential)", ErrInvalidProxyURL)
	case u.Scheme != "http" && u.Scheme != "https":
		// Split from the host check so the operator learns WHICH of the two failed
		// without the value being quoted to tell them. The scheme is named, not
		// shown: "sk-secret:tok@host" puts the credential in exactly this field.
		return nil, fmt.Errorf("%w (its scheme is neither http nor https; the value is "+
			"not quoted here because it is unvalidated and may carry a credential)", ErrInvalidProxyURL)
	case u.Hostname() == "":
		return nil, fmt.Errorf("%w (it has no host; the value is not quoted here because "+
			"it is unvalidated and may carry a credential)", ErrInvalidProxyURL)
	case !usablePort(u.Port()):
		// url.Parse only checks that an explicit port is DIGITS, not that it is a port
		// a socket can be opened on. "http://proxy.internal:99999" therefore parses with
		// a non-empty hostname and construction succeeds, and every Complete then fails
		// in the transport with an invalid-port error — the once-per-generation late
		// failure this constructor exists to convert into a once-at-startup one. The
		// value is NOT echoed: this branch is reached with a value that is unvalidated
		// as a whole, and a port is a component like any other.
		return nil, fmt.Errorf("%w (its port is not in the usable range 1-65535; the value "+
			"is not quoted here because it is unvalidated and may carry a credential)",
			ErrInvalidProxyURL)
	case u.User != nil, u.RawQuery != "", u.ForceQuery, hasFragment(cfg.ProxyURL):
		// notBaseComponents names components, never their values. The host is no longer
		// quoted alongside them: a hostname is as much unvalidated input as the rest.
		return nil, fmt.Errorf("%w (it carried: %s)", ErrProxyURLNotABase, notBaseComponents(u, cfg.ProxyURL))
	}
	c := &Client{
		cfg: cfg,
		// Built once here rather than per call, so the value the requests use is the
		// value construction validated — a later edit cannot reintroduce an unchecked one.
		endpoint:       strings.TrimRight(cfg.ProxyURL, "/") + "/chat/completions",
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

// usablePort reports whether a URL's explicit port can actually be dialled. An EMPTY
// port is usable — it means the scheme's default, which is the ordinary case. url.Parse
// has already rejected a non-numeric port, so Atoi fails here only on a value too large
// for an int, which is out of range by definition.
func usablePort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return n >= 1 && n <= 65535
}

// notBaseComponents names which disallowed components a proxy URL carried, so the
// operator can fix it without the message reproducing any of their VALUES.
func notBaseComponents(u *url.URL, raw string) string {
	var found []string
	if u.User != nil {
		found = append(found, "userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		found = append(found, "query")
	}
	if hasFragment(raw) {
		found = append(found, "fragment")
	}
	return strings.Join(found, ", ")
}

// hasFragment reports whether raw carries a fragment DELIMITER, empty value included.
//
// `u.Fragment != ""` asks whether the fragment has content, and content is not what breaks
// the endpoint. `https://proxy/v1#` parses to an empty Fragment, so that test passes, and
// then `endpoint` is built by concatenation: `https://proxy/v1#/chat/completions`, whose
// path is `/v1` with everything after the `#` a fragment the transport never sends. Every
// generation would post to the wrong endpoint — the recurring shape on this constructor,
// a value url.Parse accepts and the transport cannot use.
//
// net/url records the analogous empty QUERY in ForceQuery and has no ForceFragment, so the
// delimiter is looked for in the raw value. That is exact rather than approximate: `#` is
// the fragment delimiter wherever it appears unescaped, everything after the first one is
// the fragment, and a `#` meant literally would be `%23`.
func hasFragment(raw string) bool {
	return strings.Contains(raw, "#")
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
	if ferr := finishReasonErr(resp.Choices[0].FinishReason); ferr != nil {
		return "", ferr
	}
	return resp.Choices[0].Message.Content, nil
}

// finishReasonErr rejects a completion the model stopped short of finishing.
//
// The field was previously decoded and DISCARDED, which made "length" — the max_tokens
// truncation — indistinguishable from a clean answer at every call site, and removed the
// only signal a caller could have used to refuse partial copy. Returning an error rather
// than the reason keeps Complete's signature at (string, error). The contract this sets
// for the part-2 consumer is that a truncated completion is unusable — it must not fall
// back to the returned string — and one that later wants the reason itself can match the
// sentinel. This package has no non-test caller yet, so nothing exercises that path
// today.
//
// An EMPTY reason is accepted. The field is optional in practice — OpenAI-compatible
// proxies fronting other providers do omit it — and the content has already been checked
// non-empty, so rejecting on absence would make this client unusable against a working
// deployment to protect against a case it cannot detect anyway.
//
// A reason this function does not recognise is named as unrecognised rather than quoted.
// It is model-adjacent text arriving over the wire, and the rule this package holds is
// that a component is safe to print only where the code has compared it to a constant
// first — which is exactly what the named cases below have done and the default has not.
func finishReasonErr(reason string) error {
	switch strings.TrimSpace(reason) {
	case "", "stop":
		return nil
	case "length":
		return fmt.Errorf("%w (it hit the max_tokens budget and was cut off mid-output)",
			ErrIncompleteCompletion)
	case "content_filter":
		return fmt.Errorf("%w (a content filter stopped the model)", ErrIncompleteCompletion)
	case "tool_calls", "function_call":
		return fmt.Errorf("%w (the model asked to call a tool, and this client offers none, "+
			"so what came back is not the answer)", ErrIncompleteCompletion)
	default:
		return fmt.Errorf("%w (for a reason this client does not recognise; the value is not "+
			"quoted here because it is text the model controls)", ErrIncompleteCompletion)
	}
}

// do performs the POST with per-attempt deadlines and bounded 429 retry.
func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= retryMax; attempt++ {
		// Checked BEFORE the attempt, and not inferred from the remaining budget: a
		// context cancelled without a deadline still reports a full budget, so a
		// caller who has gone away would otherwise get one more request made for them.
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("llm: %w", cerr)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
		data, retryAfter, err := c.attempt(attemptCtx, c.endpoint, body)
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

	// Read ONE byte past the cap so a body of exactly maxResponseBody is
	// distinguishable from a larger one truncated at it. io.LimitReader signals the
	// limit with EOF, not an error, so a plain LimitReader(cap) hands back a truncated
	// prefix indistinguishable from a complete body — and a prefix can still be valid
	// JSON (a complete completion object followed by padding, cut before whatever came
	// after) and would then be accepted as the whole answer. Mirrors the
	// LinkedIn/Meta/Reddit/Twitter clients' maxResponseBody+1 boundary.
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBody+1)); err != nil {
		return nil, -1, fmt.Errorf("llm: read response: %w", redactTransport(err))
	}
	if buf.Len() > maxResponseBody {
		return nil, -1, fmt.Errorf("llm: proxy response exceeds the %d-byte cap", maxResponseBody)
	}
	return buf.Bytes(), -1, nil
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
//
// A delta-seconds value that OVERFLOWS is the one case where zero is the wrong
// answer, and it is why this no longer leans on time.ParseDuration. An all-digit
// header too large for a Duration ("99999999999999999999999999") means the proxy is
// declaring a reset far beyond anything worth waiting for — but ParseDuration fails
// on it, and a failure here reads as "no header", which sends the caller into
// ordinary exponential backoff and a retry the server has already refused. Such a
// value is classified as OVER the cap so `wait > maxRetryWait` aborts, matching the
// Microsoft sibling. Digits-only is also the delta-seconds grammar (RFC 9110 §10.2.3);
// ParseDuration additionally accepted shapes like "1h2m0" that the header never has.
func (c *Client) retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if isAllDigits(v) {
		// Compared in SECONDS before converting: secs * time.Second overflows and can
		// wrap to a non-positive Duration for a large value, which would silently skip
		// the abort the comparison exists to trigger.
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(maxRetryWait/time.Second) {
			// err here can only be a range error — the digits already parsed as digits.
			return maxRetryWait + time.Second
		}
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d
		}
	}
	return 0
}

// isAllDigits reports whether s is a non-empty ASCII digit string — the
// delta-seconds grammar. Used to tell an overflowing delta-seconds value (abort)
// from a header in some other shape (fall through to the HTTP-date form).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
