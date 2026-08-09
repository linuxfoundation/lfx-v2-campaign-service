// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testKey = "sk-super-secret-proxy-key"

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]Option{
		WithHTTPClient(srv.Client()),
		WithRetryBaseDelay(time.Millisecond),
	}, opts...)
	c, err := NewClient(Config{ProxyURL: srv.URL, APIKey: testKey}, all...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// completion renders the proxy's success shape. Built by marshalling the text so a
// value needing escaping cannot produce invalid JSON.
func completion(text string) string {
	q, _ := json.Marshal(text)
	return `{"choices":[{"message":{"content":` + string(q) + `},"finish_reason":"stop"}]}`
}

// reqRecorder captures what a fake handler saw. The handler runs on the SERVER's
// goroutine and the test goroutine reads the captures after Complete returns; that
// pairing is ordered in practice but carries no happens-before edge, so -race reports
// it as a data race. Guarding it here matches the mutex/atomic discipline the retry
// tests in this file already use.
type reqRecorder struct {
	mu   sync.Mutex
	req  chatRequest
	auth string
	path string
}

// capture records the request. It decodes the body, so a handler must call it before
// writing its response.
func (r *reqRecorder) capture(hr *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = hr.URL.Path
	r.auth = hr.Header.Get("Authorization")
	_ = json.NewDecoder(hr.Body).Decode(&r.req)
}

func (r *reqRecorder) Req() chatRequest { r.mu.Lock(); defer r.mu.Unlock(); return r.req }
func (r *reqRecorder) Auth() string     { r.mu.Lock(); defer r.mu.Unlock(); return r.auth }
func (r *reqRecorder) Path() string     { r.mu.Lock(); defer r.mu.Unlock(); return r.path }

func TestComplete_SendsOpenAIShapeAndReturnsContent(t *testing.T) {
	rec := &reqRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("hello"))
	})

	out, err := c.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello" {
		t.Errorf("content = %q, want %q", out, "hello")
	}
	if got := rec.Path(); got != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got)
	}
	if got := rec.Auth(); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q", got)
	}
	got := rec.Req()
	if got.Model != DefaultModel {
		t.Errorf("model = %q, want %q", got.Model, DefaultModel)
	}
	if got.Temperature != DefaultTemperature || got.MaxTokens != DefaultMaxTokens {
		t.Errorf("temperature/max_tokens = %v/%d", got.Temperature, got.MaxTokens)
	}
	want := []chatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "usr"}}
	if len(got.Messages) != 2 || got.Messages[0] != want[0] || got.Messages[1] != want[1] {
		t.Errorf("messages = %+v, want %+v", got.Messages, want)
	}
}

// A zero temperature is a meaningful request, which is why Config carries a
// *float64: a plain float64 would send DefaultTemperature here.
func TestComplete_ZeroTemperatureIsHonouredNotTreatedAsUnset(t *testing.T) {
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("ok"))
	}))
	defer srv.Close()

	zero := 0.0
	c, err := NewClient(
		Config{ProxyURL: srv.URL, APIKey: testKey, Temperature: &zero, Model: "custom-model", MaxTokens: 99},
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := rec.Req()
	if got.Temperature != 0 {
		t.Errorf("temperature = %v, want 0 (an explicit zero must not fall back to the default)", got.Temperature)
	}
	if got.Model != "custom-model" || got.MaxTokens != 99 {
		t.Errorf("model/max_tokens = %q/%d, want custom-model/99", got.Model, got.MaxTokens)
	}
}

func TestNewClient_RequiresProxyURLAndKey(t *testing.T) {
	for _, tc := range []struct{ name, url, key string }{
		{"no url", "", "k"},
		{"no key", "http://x", ""},
		{"blank url", "   ", "k"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(Config{ProxyURL: tc.url, APIKey: tc.key}); !errors.Is(err, ErrNotConfigured) {
				t.Errorf("err = %v, want ErrNotConfigured", err)
			}
		})
	}
}

// TestNewClient_RejectsAProxyURLThatIsNotAbsoluteHTTP covers the values that are present
// but unusable — the class the emptiness check above cannot see. "localhost:4000" is the
// one that matters: url.Parse accepts it, reading "localhost" as the SCHEME, so nothing
// complains until http.NewRequest refuses it on every single generation. A one-line
// deployment mistake then reads as a recurring transport failure rather than as
// misconfiguration, which is precisely what constructing eagerly exists to prevent.
//
// Each must satisfy errors.Is(ErrNotConfigured) as well, because every caller degrades on
// that sentinel and an unusable proxy is operationally identical to an absent one. A
// separate sentinel here would silently stop them degrading.
func TestNewClient_RejectsAProxyURLThatIsNotAbsoluteHTTP(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"host and port, no scheme", "localhost:4000"},
		{"scheme only", "http://"},
		// Port-only authorities are the reason the guard reads Hostname() rather than
		// Host: url.Parse gives these a NON-EMPTY Host (":4000"), and http.NewRequest
		// accepts them, so a Host check would let them through and the failure would
		// land once per generation instead of once at startup.
		{"port only", "http://:4000"},
		{"port only, https", "https://:8080"},
		{"schemeless absolute path", "/v1"},
		{"relative", "proxy/v1"},
		{"not http", "ftp://proxy.internal"},
		{"control character", "http://proxy\x7f.internal"},
		// url.Parse checks only that an explicit port is DIGITS. These parse with a
		// non-empty hostname, so without the range check construction succeeds and the
		// transport rejects them once per generated email instead.
		{"port above the tcp range", "http://proxy.internal:99999"},
		{"port zero", "http://proxy.internal:0"},
		{"port too large for an int", "http://proxy.internal:99999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(Config{ProxyURL: tc.url, APIKey: testKey})
			if !errors.Is(err, ErrInvalidProxyURL) {
				t.Fatalf("NewClient(%q) err = %v, want ErrInvalidProxyURL — an unusable proxy "+
					"url must fail at construction, not once per generated email", tc.url, err)
			}
			if !errors.Is(err, ErrNotConfigured) {
				t.Errorf("NewClient(%q) err does not satisfy ErrNotConfigured; callers degrade "+
					"on that sentinel, and an unusable proxy is the same operational fact as "+
					"an absent one", tc.url)
			}
		})
	}
}

// TestNewClient_RejectsAProxyURLCarryingUserinfoOrQuery pins the second reason a present
// value can be unusable: it is a whole URL where a BASE url was required. The endpoint is
// built by appending "/chat/completions", so a value ending in "?x=1" or "#f" yields a path
// nobody wrote, and userinfo is a second credential channel this client neither asked for
// nor manages. Both are also exactly where a token hides inside a URL, which is why the
// message may not quote the value — the assertions below pin that too.
func TestNewClient_RejectsAProxyURLCarryingUserinfoOrQuery(t *testing.T) {
	const secret = "sup3r-s3cret" // secretlint-disable-line -- fixture asserting it is not echoed
	for _, tc := range []struct{ name, url, component string }{
		{"userinfo password", "https://user:" + secret + "@litellm.internal/v1", "userinfo"},
		{"userinfo user only", "https://" + secret + "@litellm.internal/v1", "userinfo"},
		{"query api key", "https://litellm.internal/v1?api-key=" + secret, "query"},
		{"empty forced query", "https://litellm.internal/v1?", "query"},
		{"fragment", "https://litellm.internal/v1#" + secret, "fragment"},
		{"userinfo and query", "https://u:" + secret + "@litellm.internal/v1?k=" + secret, "userinfo, query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(Config{ProxyURL: tc.url, APIKey: testKey})
			if !errors.Is(err, ErrProxyURLNotABase) {
				t.Fatalf("NewClient(%q) err = %v, want ErrProxyURLNotABase", tc.name, err)
			}
			// Degrading callers match the two outer sentinels, so the new one must
			// remain reachable through both or an unusable proxy stops degrading.
			if !errors.Is(err, ErrInvalidProxyURL) || !errors.Is(err, ErrNotConfigured) {
				t.Errorf("err does not satisfy ErrInvalidProxyURL/ErrNotConfigured: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error message echoes the rejected value's secret component: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.component) {
				t.Errorf("error names %q, want it to name the offending component %q", err.Error(), tc.component)
			}
			// The host is NOT named, and this assertion is the inverse of what it was.
			// It used to require the host, on the reasoning that url.Parse had already
			// split userinfo out of it so it was safe AND useful. That is wrong for the
			// same reason the scheme is: url.Parse decides where the delimiters fall,
			// not what a component holds — a pasted credential lands in the host just as
			// readily (see the "secret mistaken for a host" case below). The operator
			// locates the value from the variable name in the wrapped sentinel, which
			// costs nothing and reproduces nothing.
			if strings.Contains(err.Error(), "litellm.internal") {
				t.Errorf("error %q names the host; no component of a rejected value may be quoted", err.Error())
			}
		})
	}
}

// TestNewClient_RejectionNeverEchoesTheRawURL covers the OTHER rejection paths for the same
// property. The parse-failure branch is the subtle one: url.Parse returns a *url.Error whose
// Error() embeds the raw url verbatim, so simply wrapping it re-publishes a value that may
// carry a token — and the wrapping looked entirely idiomatic. Unwrapping to the bare cause
// is only most of the fix, which is what the malformed-port case below pins: net/url's
// causes quote the FRAGMENT they choked on, so the cause is discarded outright.
func TestNewClient_RejectionNeverEchoesTheRawURL(t *testing.T) {
	const secret = "sup3r-s3cret" // secretlint-disable-line -- fixture asserting it is not echoed
	for _, tc := range []struct{ name, url string }{
		// Unparseable: "%zz" is an invalid escape, so url.Parse fails with the raw
		// value — secret included — inside the *url.Error it returns.
		{"parse failure", "https://user:" + secret + "@litellm.internal/%zz"},
		// The case unwrapping does NOT fix. No "@", so the secret is read as a PORT,
		// and the inner cause is `invalid port ":sup3r-s3cret" after host` — the
		// credential quoted by the cause itself, not by the *url.Error wrapper.
		{"secret mistaken for a port", "https://litellm.internal:" + secret},
		// Wrong scheme, but userinfo still present: takes the scheme branch, which
		// must report the components it JUDGED rather than the value it read.
		{"bad scheme with userinfo", "ftp://user:" + secret + "@litellm.internal"},
		{"schemeless with userinfo", "user:" + secret + "@litellm.internal"},
		// The credential IS the scheme, and the value parses cleanly with a host —
		// so no parse-failure guard fires and the scheme branch is reached with the
		// secret sitting in u.Scheme. This is the same lesson as the port case one
		// layer out: url.Parse decides where the delimiters fall, not what a
		// component contains, so no component of a rejected value can be quoted.
		{"secret mistaken for a scheme", secret + "://litellm.internal"},
		{"secret as an opaque scheme", secret + ":tok@litellm.internal"},
		// The credential as the HOST, on the not-a-base branch, which quoted u.Host.
		{"secret mistaken for a host", "https://" + secret + "/?k=v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(Config{ProxyURL: tc.url, APIKey: testKey})
			// Either rejection sentinel is acceptable; which branch fires is not the
			// property under test. That the message stays silent about the value is.
			if !errors.Is(err, ErrInvalidProxyURL) && !errors.Is(err, ErrProxyURLNotABase) {
				t.Fatalf("err = %v, want a proxy-url rejection sentinel", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("rejection echoed the credential: %q", err.Error())
			}
		})
	}
}

// TestNewClient_AcceptsAUsableProxyURL is the other side, and it exists so the guard above
// cannot be satisfied by rejecting everything. A path prefix is the real deployment shape
// (the proxy is mounted at /v1), and a trailing slash must not produce a doubled separator.
func TestNewClient_AcceptsAUsableProxyURL(t *testing.T) {
	for _, u := range []string{
		"http://litellm:4000",
		"https://litellm.internal/v1",
		"https://litellm.internal/v1/",
	} {
		if _, err := NewClient(Config{ProxyURL: u, APIKey: testKey}); err != nil {
			t.Errorf("NewClient(%q) = %v, want a client", u, err)
		}
	}
}

// TestNewClient_NormalizesPaddedConfigOnTheWire is the other half of the guard above.
// TrimSpace used only for the emptiness CHECK admits every padded value that is not
// entirely whitespace, and each field then fails somewhere less legible than construction:
// a key with a trailing newline builds an Authorization header Go's transport rejects as
// an invalid header value, and a padded URL stops parsing as the URL it looks like. Both
// surface at generation time as a request failure rather than as the misconfiguration they
// are — and a trailing newline is the single most common way a Kubernetes secret arrives
// malformed, so this is the reachable case, not the exotic one.
//
// Asserted on the WIRE rather than on the struct: the fields are unexported, and what
// matters is the header and path the proxy actually receives.
func TestNewClient_NormalizesPaddedConfigOnTheWire(t *testing.T) {
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("ok"))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(
		Config{ProxyURL: "  " + srv.URL + "\n", APIKey: "  " + testKey + "\n", Model: " some-model \n"},
		WithHTTPClient(srv.Client()), WithRetryBaseDelay(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient with padded config: %v — padding is a misconfiguration to "+
			"normalize, not one to reject: the operator supplied the right values", err)
	}
	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Complete: %v — a padded key or URL must not reach the transport", err)
	}

	if got, want := rec.Auth(), "Bearer "+testKey; got != want {
		t.Errorf("Authorization = %q, want %q — an untrimmed key builds a header the "+
			"transport rejects, and the failure reads as an outage rather than a config error",
			got, want)
	}
	if got := rec.Req().Model; got != "some-model" {
		t.Errorf("model = %q, want %q — a padded model id routes to nothing on the proxy "+
			"while looking correct in a log", got, "some-model")
	}
}

// A model that is ONLY whitespace must fall back to DefaultModel rather than being sent as
// an empty string, which the proxy would reject. Normalizing at construction is what makes
// the two cases one case.
func TestNewClient_WhitespaceOnlyModelFallsBackToTheDefault(t *testing.T) {
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("ok"))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{ProxyURL: srv.URL, APIKey: testKey, Model: "   "},
		WithHTTPClient(srv.Client()), WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := rec.Req().Model; got != DefaultModel {
		t.Errorf("model = %q, want DefaultModel %q", got, DefaultModel)
	}
}

func TestComplete_EmptyChoicesIsDistinctFromTransportFailure(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no choices", `{"choices":[]}`},
		{"blank content", `{"choices":[{"message":{"content":"   "}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.Complete(context.Background(), "s", "u")
			if !errors.Is(err, ErrEmptyCompletion) {
				t.Errorf("err = %v, want ErrEmptyCompletion", err)
			}
		})
	}
}

func TestComplete_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "slow down")
			return
		}
		_, _ = io.WriteString(w, completion("second try"))
	})

	out, err := c.Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "second try" {
		t.Errorf("content = %q", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

// A 429 body must be DRAINED before Close or net/http will not pool the connection
// and the retry reopens TCP+TLS. Counting distinct remote addresses is what proves
// reuse; asserting an io.Discard call would restate the implementation.
func TestComplete_RetryReusesTheConnection(t *testing.T) {
	var mu sync.Mutex
	addrs := map[string]bool{}
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		addrs[r.RemoteAddr] = true
		mu.Unlock()
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			// A body long enough that an undrained close would abandon the connection.
			_, _ = io.WriteString(w, strings.Repeat("rate limited. ", 200))
			return
		}
		_, _ = io.WriteString(w, completion("ok"))
	})

	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	mu.Lock()
	n := len(addrs)
	mu.Unlock()
	if n != 1 {
		t.Errorf("server saw %d client connections, want 1: the 429 body was not drained "+
			"before Close, so the retry could not reuse the pooled connection", n)
	}
}

func TestComplete_RetryAfterSecondsIsHonoured(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1000") // seconds -> above maxRetryWait
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, completion("never reached"))
	})

	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("want an error: a Retry-After beyond maxRetryWait must abort, not sleep")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("calls = %d, want 1 (no retry after an over-long Retry-After)", n)
	}
}

func TestComplete_RetryAfterHTTPDateIsParsed(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	got := (&Client{now: func() time.Time { return base }}).retryAfter(base.Add(3 * time.Second).Format(http.TimeFormat))
	// http.TimeFormat has second granularity, so allow the truncation slack.
	if got < 2*time.Second || got > 3*time.Second {
		t.Errorf("retryAfter = %v, want ~3s", got)
	}
	if d := (&Client{now: func() time.Time { return base }}).retryAfter("not a date"); d != 0 {
		t.Errorf("unparseable Retry-After = %v, want 0 (back off on our own schedule, not refuse)", d)
	}
}

func TestComplete_GivesUpAfterRetryMax(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("want an error after exhausting retries")
	}
	if n, want := atomic.LoadInt32(&calls), int32(retryMax+1); n != want {
		t.Errorf("calls = %d, want %d", n, want)
	}
}

// The final 429 must not be followed by a backoff. There are no attempts left to wait
// FOR, so sleeping only delays a known-final error — by up to maxRetryWait when the
// server sent a large Retry-After — while the caller's own deadline burns down.
//
// Only the LAST response carries a Retry-After, so the two legitimate inter-attempt
// waits stay on the cheap exponential schedule (1s + 2s) and the 10s is reachable ONLY
// from the after-the-final-attempt sleep. That keeps the test fast and makes the
// measurement unambiguous rather than a wall-clock guess.
func TestComplete_DoesNotSleepAfterTheFinalAttempt(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == int32(retryMax+1) {
			// Under maxRetryWait, so an unfixed client really sleeps it rather than
			// aborting — which would pass this test for the wrong reason.
			w.Header().Set("Retry-After", "10")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})

	start := time.Now()
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("want an error after exhausting retries")
	}
	if n, want := atomic.LoadInt32(&calls), int32(retryMax+1); n != want {
		t.Fatalf("calls = %d, want %d", n, want)
	}
	// ~3s of real backoff (1s + 2s) plus generous headroom; the bug adds 10s.
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("Complete took %v — it slept after the final attempt", elapsed)
	}
}

func TestComplete_Non429ErrorIsNotRetried(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "prompt echoed back: SECRET-CONTEXT")
	})

	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("want an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
	if strings.Contains(err.Error(), "SECRET-CONTEXT") {
		t.Errorf("error quotes the proxy's body, which can carry the prompt: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should name the status: %v", err)
	}
}

func TestComplete_RedirectIsNotFollowed(t *testing.T) {
	var elsewhereHit int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&elsewhereHit, 1)
		if r.Header.Get("Authorization") != "" {
			t.Error("the bearer credential was resent to the redirect target")
		}
		_, _ = io.WriteString(w, completion("attacker"))
	}))
	defer elsewhere.Close()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/chat/completions", http.StatusTemporaryRedirect)
	})

	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("want an error: a 3xx from the proxy must not be followed")
	}
	if n := atomic.LoadInt32(&elsewhereHit); n != 0 {
		t.Errorf("redirect target was contacted %d times, want 0", n)
	}
}

// The per-attempt deadline must come from a context, not only http.Client.Timeout:
// an injected client with no Timeout (as here) would otherwise hang for as long as
// the caller's context allows.
func TestComplete_PerAttemptTimeoutIsEnforcedWithoutClientTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}, WithRequestTimeout(30*time.Millisecond))
	if c.httpClient.Timeout != 0 {
		t.Fatalf("precondition: test client should have no Timeout, got %v", c.httpClient.Timeout)
	}

	done := make(chan error, 1)
	go func() { _, err := c.Complete(context.Background(), "s", "u"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want a timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not honour its per-attempt timeout")
	}
}

// ctx.Err() must be consulted before an attempt starts: a context cancelled WITHOUT
// a deadline still reports a full budget.
func TestComplete_CancelledContextIsCheckedBeforeAnyRequest(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, completion("ok"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Complete(ctx, "s", "u"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
}

func TestComplete_ResponseBodyIsBounded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Valid JSON prefix then far more than maxResponseBody of filler.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`)
		chunk := strings.Repeat("A", 1<<20)
		for i := 0; i < 12; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}}]}`)
	})

	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("want a decode error from the truncated (bounded) read")
	}
}

func TestComplete_ProxyURLTrailingSlashDoesNotDoubleUp(t *testing.T) {
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("ok"))
	}))
	defer srv.Close()

	c, err := NewClient(Config{ProxyURL: srv.URL + "/", APIKey: testKey}, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := rec.Path(); got != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", got)
	}
}

// credentialLeakingTransport reproduces the shape the redaction exists for: a
// RoundTripper that renders the request (headers included) into its error. The
// credential-bearing layer here is a *fmt.wrapError, NOT a *url.Error, so peeling
// url.Error layers would leave it reachable.
type credentialLeakingTransport struct{ cause error }

func (t credentialLeakingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dialing with %s: %w", r.Header.Get("Authorization"), t.cause)
}

func TestComplete_TransportErrorLeaksNothingAtAnyLayer(t *testing.T) {
	causes := map[string]error{
		"dns":      &net.DNSError{Err: "no such host", Name: "proxy.internal", IsNotFound: true},
		"refused":  syscall.ECONNREFUSED,
		"reset":    syscall.ECONNRESET,
		"timeout":  syscall.ETIMEDOUT,
		"unknown":  errors.New("something bespoke"),
		"deadline": context.DeadlineExceeded,
	}
	for name, cause := range causes {
		t.Run(name, func(t *testing.T) {
			c, err := NewClient(
				Config{ProxyURL: "https://proxy.internal", APIKey: testKey},
				WithHTTPClient(&http.Client{Transport: credentialLeakingTransport{cause: cause}}),
				WithRetryBaseDelay(time.Millisecond),
			)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = c.Complete(context.Background(), "s", "u")
			if err == nil {
				t.Fatal("want an error")
			}
			// EVERY layer, not just the outermost Error(): an errors.As walk
			// can reach an inner layer the top-level string hides.
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), testKey) {
					t.Fatalf("layer %T leaks the API key: %v", e, e)
				}
			}
		})
	}
}

func TestRedactTransport_PreservesTheSentinelsCallersMatchOn(t *testing.T) {
	for name, cause := range map[string]error{
		"refused":  syscall.ECONNREFUSED,
		"deadline": context.DeadlineExceeded,
		"canceled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			got := redactTransport(fmt.Errorf("Bearer %s: %w", testKey, cause))
			if !errors.Is(got, cause) {
				t.Errorf("errors.Is(redacted, %v) = false; redaction must keep the sentinel matchable", cause)
			}
			if strings.Contains(got.Error(), testKey) {
				t.Errorf("redacted error still carries the key: %v", got)
			}
		})
	}

	// A DNS error keeps its boolean bits (callers branch on IsNotFound) but is
	// rebuilt, so nothing the resolver attached survives.
	got := redactTransport(fmt.Errorf("Bearer %s: %w", testKey,
		&net.DNSError{Err: "leaky " + testKey, Name: "h", IsNotFound: true}))
	var dnsErr *net.DNSError
	if !errors.As(got, &dnsErr) {
		t.Fatalf("want a *net.DNSError, got %T", got)
	}
	if !dnsErr.IsNotFound {
		t.Error("IsNotFound was dropped by the rebuild")
	}
	if strings.Contains(dnsErr.Error(), testKey) {
		t.Errorf("rebuilt DNS error carries the key: %v", dnsErr)
	}

	if redactTransport(nil) != nil {
		t.Error("redactTransport(nil) must be nil")
	}
}

// An over-cap body whose first maxResponseBody bytes are VALID JSON is the case a
// plain LimitReader(cap) cannot see. io.LimitReader signals the limit with EOF, so
// the truncated prefix arrives looking exactly like a complete response — and here it
// parses, so nothing downstream notices either. The read goes one byte past the cap
// precisely so "exactly at the cap" and "larger, cut at the cap" stay distinguishable.
func TestComplete_OverCapBodyIsRejectedEvenWhenTheTruncatedPrefixParses(t *testing.T) {
	body := completion("ok")
	// Pad with spaces to exactly the cap: json.Unmarshal accepts trailing whitespace,
	// so the first maxResponseBody bytes are a complete, valid completion.
	pad := strings.Repeat(" ", maxResponseBody-len(body))

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
		_, _ = io.WriteString(w, pad)
		// Past the cap. A LimitReader(cap) never sees this and reports success.
		_, _ = io.WriteString(w, "trailing garbage the client must not silently drop")
	})

	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("want an error: a body larger than the cap was accepted because its truncated prefix happened to parse")
	}
}

// A body of EXACTLY the cap is complete, not truncated, and must still succeed —
// the +1 read is a boundary check, not a tightening of the limit by one byte.
func TestComplete_BodyExactlyAtTheCapIsAccepted(t *testing.T) {
	body := completion("ok")
	pad := strings.Repeat(" ", maxResponseBody-len(body))

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
		_, _ = io.WriteString(w, pad)
	})

	got, err := c.Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("Complete: %v (a body of exactly maxResponseBody is complete, not over-cap)", err)
	}
	if got != "ok" {
		t.Errorf("content = %q, want %q", got, "ok")
	}
}

// An all-digit Retry-After too large for a Duration must read as OVER the cap, not
// as an absent header. Treating it as absent sends the caller into ordinary
// exponential backoff and a retry the proxy has already refused.
func TestRetryAfter_OverflowingDeltaSecondsAbortsRatherThanRetrying(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	c := &Client{now: func() time.Time { return base }}

	for _, tc := range []struct {
		name, header string
		want         func(time.Duration) bool
		wantDesc     string
	}{
		{"overflows int64", "99999999999999999999999999", func(d time.Duration) bool { return d > maxRetryWait }, "> maxRetryWait"},
		// Just inside what a Duration can hold (~292 years in seconds), so this one is
		// over-cap by the ordinary comparison rather than by the overflow branch. It is
		// here to pin that the two paths agree at the boundary.
		{"largest representable delta-seconds", "9223372036", func(d time.Duration) bool { return d > maxRetryWait }, "> maxRetryWait"},
		{"ordinary over-cap", "1000", func(d time.Duration) bool { return d > maxRetryWait }, "> maxRetryWait"},
		{"ordinary under-cap", "5", func(d time.Duration) bool { return d == 5*time.Second }, "5s"},
		{"zero", "0", func(d time.Duration) bool { return d == 0 }, "0"},
		{"not digits and not a date", "soon", func(d time.Duration) bool { return d == 0 }, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.retryAfter(tc.header); !tc.want(got) {
				t.Errorf("retryAfter(%q) = %v, want %s", tc.header, got, tc.wantDesc)
			}
		})
	}
}

// Client documents itself safe for concurrent use, so the stored config must not
// alias memory the caller still owns. Temperature is the only pointer field.
func TestNewClient_SnapshotsTemperatureRatherThanAliasingTheCaller(t *testing.T) {
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, completion("ok"))
	}))
	defer srv.Close()

	temp := 0.2
	c, err := NewClient(
		Config{ProxyURL: srv.URL, APIKey: testKey, Temperature: &temp},
		WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The caller reuses its own variable after construction. With the pointer aliased,
	// this silently changes what every subsequent Complete sends — and concurrently
	// with one in flight, it is a data race.
	temp = 0.9

	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := rec.Req().Temperature; got != 0.2 {
		t.Errorf("temperature = %v, want 0.2 (the value at construction; the client aliased the caller's variable)", got)
	}
}

// TestComplete_RefusesACompletionTheModelDidNotFinish pins the contract chosen for
// finish_reason, which was previously decoded and thrown away. "length" is the case that
// matters: max_tokens truncated the output, so what came back is real, fluent, and PARTIAL
// — half an email reads like an email, which is why a nil error here is worse than an
// empty response. The default branch must not quote the value; it is text the model
// controls, and this package only prints components it has compared to a constant.
func TestComplete_RefusesACompletionTheModelDidNotFinish(t *testing.T) {
	const modelText = "sup3r-s3cret-reason" // secretlint-disable-line -- fixture asserting it is not echoed
	for _, tc := range []struct {
		name, reason string
		wantErr      bool
	}{
		{"stop is the finished answer", "stop", false},
		{"absent is accepted; the field is optional in practice", "", false},
		{"length means truncated mid-output", "length", true},
		{"content filter stopped it", "content_filter", true},
		{"the model wanted a tool this client does not offer", "tool_calls", true},
		{"an unrecognised reason fails closed", modelText, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				q, _ := json.Marshal(tc.reason)
				_, _ = io.WriteString(w,
					`{"choices":[{"message":{"content":"a whole email, apparently"},"finish_reason":`+
						string(q)+`}]}`)
			})
			out, err := c.Complete(context.Background(), "s", "u")
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("finish_reason %q: Complete = %v, want the content returned", tc.reason, err)
				}
				if out != "a whole email, apparently" {
					t.Errorf("content = %q, want it returned unchanged", out)
				}
				return
			}
			if !errors.Is(err, ErrIncompleteCompletion) {
				t.Fatalf("finish_reason %q: err = %v, want ErrIncompleteCompletion — partial copy "+
					"returned with a nil error is indistinguishable from a finished answer", tc.reason, err)
			}
			if out != "" {
				t.Errorf("finish_reason %q returned content %q alongside the error; a caller that "+
					"ignores the error would send truncated copy", tc.reason, out)
			}
			if errors.Is(err, ErrEmptyCompletion) {
				t.Errorf("finish_reason %q must not read as ErrEmptyCompletion: there IS content, "+
					"and the two failures call for different handling", tc.reason)
			}
			if strings.Contains(err.Error(), modelText) {
				t.Errorf("the unrecognised reason was echoed into %q; it is model-controlled text", err)
			}
		})
	}
}
