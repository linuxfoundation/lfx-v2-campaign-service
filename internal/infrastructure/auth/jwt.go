// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package auth verifies the Heimdall-issued bearer token on every authenticated request
// and turns its claims into the domain actor recorded on writes.
//
// Heimdall already validates these tokens, so this is not here to make the happy path
// work — it is here because the gateway's guarantee stops at the cluster boundary.
// Anything reaching the pod directly (a misconfigured NetworkPolicy, a sibling workload,
// a port-forward) bypasses it, and these claims are written to created_by / updated_by.
// An unverified claim is therefore not merely an unauthenticated request: it is a
// forgeable audit trail for who authorized paid ad spend.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
	"strings"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"github.com/auth0/go-jwt-middleware/v2/validator"
	"golang.org/x/sync/singleflight"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// PINNED, never read from the token header: trusting the declared alg is what makes
	// algorithm-confusion attacks work. The TTL's refetch on expiry is what lets a
	// Heimdall key rotation take effect without restarting this pod.
	signatureAlgorithm = validator.PS256
	jwksCacheTTL       = 5 * time.Minute
	clockSkew          = 5 * time.Second // exp/nbf drift, as the sibling services allow
	bearerPrefix       = "bearer "       // stripped case-insensitively
	// jwksFetchTimeout bounds the key fetch. The provider defaults to a client with NO
	// timeout, and the cold fetch runs while holding the provider's write lock — a stalled
	// Heimdall would block every authentication on this pod for as long as the TCP
	// connection hangs, which no HTTP server timeout cancels.
	jwksFetchTimeout = 10 * time.Second
)

// ErrUnauthenticated is returned for every token this package refuses — one sentinel on
// purpose: a bad signature, wrong audience, expired token and missing principal are
// indistinguishable from outside, so an attacker learns only "no".
var ErrUnauthenticated = errors.New("the bearer token is not valid for this service")

// ErrKeyUnavailable is domain.ErrKeyUnavailable, re-exported so this package's callers
// and tests name it where they name ErrUnauthenticated. It reports that Heimdall's signing
// keys could not be retrieved — a fetch that failed, timed out, or was cancelled — which is
// NOT a statement about the token: nothing was learned about it, because it was never
// checked. The service layer maps it to 503 while every token-side refusal gets 400.
var ErrKeyUnavailable = domain.ErrKeyUnavailable

// Config carries the deployment's JWT settings. An empty JWKS URL, audience or issuer
// falls back to the pkg/constants default — the value LoadConfig supplies — so a
// hand-built Config yields a verifier that verifies rather than one refusing all.
type Config struct {
	JWKSURL  string // Heimdall's JSON Web Key Set endpoint
	Audience string // the audience claim this service requires
	Issuer   string // the issuer claim this service requires
	// MockLocalPrincipal, when non-empty, DISABLES verification and treats every request
	// as this principal. Local development only.
	MockLocalPrincipal string
	// InCluster reports that this process is running as a Kubernetes pod, in which case
	// MockLocalPrincipal is refused rather than honoured. See New.
	InCluster bool
}

// heimdallClaims are Heimdall's custom claims. Principal is the LFID username (the
// platform's canonical identity); the rest are best-effort display fields.
type heimdallClaims struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Validate runs after signature, audience, issuer and expiry pass. A token with no
// principal is refused rather than accepted with a blank actor, since a VERIFIED token
// attributing a write to nobody carries the authority of having been checked.
func (c *heimdallClaims) Validate(_ context.Context) error {
	if strings.TrimSpace(c.Principal) == "" {
		return errors.New("principal claim is empty")
	}
	return nil
}

// Verifier validates bearer tokens against Heimdall's JWKS.
type Verifier struct {
	validator *validator.Validator // nil exactly when mock is non-nil
	mock      *model.Actor         // the fixed actor of local-development mock mode
}

// New builds a Verifier, erroring rather than returning a degraded one when the JWKS URL is
// unusable: a degraded verifier has two behaviours and both are wrong — refusing everything
// is a confusing outage, allowing everything is the hole this package closes.
func New(cfg Config) (*Verifier, error) {
	if p := strings.TrimSpace(cfg.MockLocalPrincipal); p != "" {
		// "Local development only" was a convention, not a control. The chart declares
		// JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL under app.environment and deployment.yaml
		// renders whatever it holds, so `--set` (or an edited values file) could ship a
		// running pod that accepts ANY bearer token as this principal — with no signature
		// check, on the endpoints that spend money. The empty default and the parity test
		// keep the chart honest about its own value; neither can stop an override.
		//
		// InCluster is the discriminator, and it deliberately does NOT rest on
		// KUBERNETES_SERVICE_HOST alone — deployment.yaml renders arbitrary env names, so
		// the same override that enables this bypass could have cleared that variable too.
		// See config.runningInCluster for the two signals and the chart-side guard. A
		// laptop, `go run`, a plain container and CI trip neither, so the developer
		// workflow this exists for is untouched.
		//
		// The refusal is an error, not a silent downgrade to real verification: a deploy
		// that asked for no authentication has a broken intent, and starting anyway —
		// verifying, and serving as if nothing were wrong — leaves that intent live in a
		// values file for the next person. Fail the pod and say why.
		if cfg.InCluster {
			return nil, fmt.Errorf("%s is set to %q, but this process is running in Kubernetes: "+
				"it disables JWT verification entirely and is for local development only. Unset it "+
				"in the chart values for this deployment", constants.EnvMockLocalPrincipal, p)
		}
		return &Verifier{mock: &model.Actor{Username: p, Name: p}}, nil
	}

	// Neither branch below may render the configured URL. A URL can carry credentials in
	// its userinfo (`https://user:pass@host/...`), and these errors are returned to main.go,
	// which LOGS them — so echoing the raw value writes the secret to the log. url.Parse's
	// own error embeds the whole raw URL, which is why it is reported rather than wrapped;
	// the config key is named instead, which is what an operator actually needs to fix it.
	//
	// The printable form is redact.URL, NOT url.URL.Redacted(): Redacted() masks the password
	// and KEEPS the username, and this service treats the username in a credential-bearing
	// URL as credential material — it is issued with the password as half of one credential,
	// so printing it narrows an attacker's search. Every formatting site below uses the same
	// helper for the same reason.
	rawJWKS := orDefault(cfg.JWKSURL, constants.DefaultJWKSURL)
	jwksURL, err := url.Parse(rawJWKS)
	if err != nil {
		return nil, fmt.Errorf("%s is not a parseable URL", constants.EnvJWKSURL)
	}
	// Scheme, not just absoluteness: http.Client cannot fetch ftp:// or file://, so
	// accepting one would satisfy the fail-fast check and then refuse every request.
	if (jwksURL.Scheme != "http" && jwksURL.Scheme != "https") || jwksURL.Host == "" {
		return nil, fmt.Errorf("%s (%q) is not an absolute http(s) URL",
			constants.EnvJWKSURL, redact.URL(jwksURL))
	}
	issuer, err := url.Parse(orDefault(cfg.Issuer, constants.DefaultIssuer))
	if err != nil {
		return nil, fmt.Errorf("%s is not a parseable URL", constants.EnvIssuer)
	}
	if issuer.String() == "" {
		return nil, errors.New("JWT issuer is empty")
	}
	audience := orDefault(cfg.Audience, constants.DefaultAudience)

	// WithCustomJWKSURI is required: "heimdall" is a bare name, not an OIDC discovery URL.
	provider := jwks.NewCachingProvider(issuer, jwksCacheTTL, jwks.WithCustomJWKSURI(jwksURL),
		jwks.WithCustomClient(&http.Client{
			Timeout:   jwksFetchTimeout,
			Transport: &jwksStatusGuard{next: http.DefaultTransport},
		}))

	v, err := validator.New(
		coalesceKeyFunc(provider.KeyFunc),
		signatureAlgorithm,
		issuer.String(),
		[]string{audience},
		validator.WithCustomClaims(func() validator.CustomClaims { return &heimdallClaims{} }),
		validator.WithAllowedClockSkew(clockSkew),
	)
	if err != nil {
		return nil, fmt.Errorf("build JWT validator: %w", err)
	}
	return &Verifier{validator: v}, nil
}

// jwksErrorBodyPeek bounds how much of a non-2xx JWKS body is read before it is discarded.
// Enough to make a misconfiguration diagnosable in a log line; small enough that a hostile
// or runaway endpoint cannot make the read itself the outage.
const jwksErrorBodyPeek = 512

// jwksMaxBody bounds a 2xx JWKS body, which — unlike the error peek — has to be read in
// FULL and replayed, because the provider still needs to decode it. A real key set is a
// few keys of a few hundred bytes; a megabyte is already three orders of magnitude past
// anything Heimdall publishes, and reading without a bound would let a runaway endpoint
// turn every token verification into an allocation the process cannot refuse. Exceeding it
// is an error rather than a truncation: a truncated key set decodes to FEWER keys, which is
// the empty-set failure in slow motion.
const jwksMaxBody = 1 << 20

// jwksStatusGuard rejects a non-2xx JWKS response BEFORE the provider decodes it.
//
// go-jwt-middleware v2.3.1 does not check the status: jwks.Provider.KeyFunc goes straight
// from Client.Do to json.NewDecoder(response.Body).Decode(&jwks) (jwks/provider.go:84-93).
// jose.JSONWebKeySet has one field and ignores unknown ones, so an error object — a 404
// `{"error":"not found"}` from a wrong path, a 502 from a proxy in front of Heimdall —
// decodes CLEANLY into a key set with zero keys. The provider then returns it as a success
// and CachingProvider caches it for the full TTL. Every valid token for the next five
// minutes fails to find its signing key, and that is a verdict on the TOKEN: HTTP 400,
// "invalid bearer token", to callers whose credentials are perfectly good, for a
// misconfiguration that is entirely ours.
//
// Failing at the transport is what turns that into an error the provider propagates, so
// coalesceKeyFunc tags it ErrKeyUnavailable and the service answers 503. It also means
// nothing is cached: CachingProvider stores only successful fetches, so the next request
// retries rather than waiting out a TTL on poisoned data.
//
// # Why the empty-key-set check lives HERE and not around KeyFunc
//
// A key set with no keys in it cannot verify any token, so returning it as a success means
// every token that arrives is refused as bad — the same 400-for-our-misconfiguration
// outcome, reached by a 200 carrying `{}` or `{"keys":[]}`, by an issuer mid-rotation with
// nothing published, or by a future provider version that drops the status check some other
// way. The obvious place to catch it is a wrapper around provider.KeyFunc, and that place
// is WRONG: CachingProvider.refreshKey stores the decoded set BEFORE returning it
// (jwks/provider.go:207-221), so a wrapper runs after the empty set is already in the cache
// and every request answers 503 for the full TTL even after Heimdall recovers seconds
// later. Rejecting at the transport means refreshKey's Provider.KeyFunc returns an error,
// nothing is cached, and the very next request retries.
//
// That placement is also why this package no longer imports go-jose. A KeyFunc wrapper has
// to type-assert the provider's decoded value, and an assertion against the wrong jose
// MAJOR compiles, vets, lints and never matches — a check with nothing to show for it.
// Reading `keys` off the raw body has no version to get wrong.
type jwksStatusGuard struct{ next http.RoundTripper }

func (g *jwksStatusGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return g.checkKeys(req, resp)
	}
	// We are returning an error instead of the response, so nothing downstream will close
	// this body. Drain a bounded prefix for the diagnostic, then close: draining also lets
	// net/http return the connection to the idle pool instead of tearing down TCP+TLS,
	// which matters because a misconfigured endpoint fails on EVERY refresh.
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, jwksErrorBodyPeek))
	_ = resp.Body.Close()
	// The body is NOT quoted into the error. It is untrusted upstream text that would
	// travel into logs; the status and the URL are what identify the misconfiguration, and
	// the body goes to a debug log where its length is bounded and its origin is obvious.
	slog.Debug("JWKS endpoint returned a non-2xx response",
		"url", redact.URL(req.URL), "status", resp.StatusCode, "body_prefix", string(peek))
	return nil, fmt.Errorf("JWKS endpoint %s returned HTTP %d", redact.URL(req.URL), resp.StatusCode)
}

// checkKeys reads a 2xx JWKS body, refuses one with no keys in it, and replays the bytes
// so the provider can still decode them.
//
// The body must be replayed rather than consumed: this is a RoundTripper, and the provider
// downstream is the thing that actually builds the key set. Reading it here and handing
// back an exhausted reader would turn every fetch into an empty decode — the failure this
// is here to prevent.
//
// Only `keys` is decoded, as raw messages. Counting them needs no knowledge of what a key
// looks like, so this cannot drift with go-jose's shape the way a typed assertion would.
// A body that is not an object, or whose `keys` is not an array, fails the decode and is
// an error for the same reason a zero-length array is: nothing usable came back.
func (g *jwksStatusGuard) checkKeys(req *http.Request, resp *http.Response) (*http.Response, error) {
	// jwksMaxBody+1 so a body exactly at the bound reads short of the limit and one byte
	// over reads to it — the difference between "large but fine" and "refuse".
	buf, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read JWKS response from %s: %w", redact.URL(req.URL), err)
	}
	if len(buf) > jwksMaxBody {
		return nil, fmt.Errorf("JWKS endpoint %s returned more than %d bytes", redact.URL(req.URL), jwksMaxBody)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(buf, &doc); err != nil {
		// The body is not quoted into the error: untrusted upstream text that would travel
		// into logs. It goes to a bounded debug line instead, as the non-2xx path does.
		slog.Debug("JWKS endpoint returned an undecodable 2xx body",
			"url", redact.URL(req.URL), "body_prefix", string(buf[:min(len(buf), jwksErrorBodyPeek)]))
		return nil, fmt.Errorf("JWKS endpoint %s returned a 2xx body that is not a key set", redact.URL(req.URL))
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("JWKS endpoint %s returned no signing keys", redact.URL(req.URL))
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	return resp, nil
}

// coalesceKeyFunc collapses concurrent COLD-cache JWKS fetches into one.
//
// jwks.CachingProvider does not do this itself. On a miss its KeyFunc calls refreshKey,
// which takes the write lock and then fetches WITHOUT re-checking the cache — so N
// callers that all miss produce N serialized HTTP fetches rather than one, each bounded
// by jwksFetchTimeout, and each holding the write lock for its full duration. The Nth
// caller therefore waits roughly N × fetch, not one fetch. Authentication is on every
// request, so the queue is the whole request load.
//
// COLD cache is the whole of it, and the distinction matters because the obvious wider
// claim is false: an ordinary TTL EXPIRY does not stampede. CachingProvider holds a
// semaphore, admits exactly one refresher, and returns the STALE cached set to everyone
// else while that one runs (jwks/provider.go:166-186) — no caller blocks and no second
// fetch starts. What reaches the cold path is (a) startup, before anything is cached, and
// (b) the second-order case: when that lone background refresh FAILS, its goroutine does
// `delete(c.cache, issuer)`, discarding the stale entry that was serving everyone. The
// next N callers find an empty cache and serialize on refreshKey's write lock — so an
// endpoint that is down produces the stampede a TTL expiry alone would not, which is
// exactly when the service can least afford it.
//
// DoChan rather than Do so each caller keeps its OWN deadline. Do returns only when the
// shared call returns, which would let one caller's slow fetch outlive a later caller's
// context; here a caller whose ctx expires stops waiting while the in-flight fetch
// continues for whoever is still listening.
//
// The key is constant: this verifier has exactly one issuer, fixed at construction.
func coalesceKeyFunc(inner func(context.Context) (any, error)) func(context.Context) (any, error) {
	var group singleflight.Group
	return func(ctx context.Context) (any, error) {
		ch := group.DoChan("jwks", func() (any, error) {
			// NOT the caller's ctx. The result is shared, so binding the fetch to whichever
			// caller happened to arrive first would let that one's cancellation fail every
			// other caller waiting on it. The provider's own http.Client timeout
			// (jwksFetchTimeout) is what bounds this.
			return inner(context.WithoutCancel(ctx))
		})
		// Both arms tag ErrKeyUnavailable, and this is the ONLY place that can: the
		// validator wraps whatever the key func returns (validator.go:192, with %w), so
		// tagging here is what lets VerifyActor tell "we could not fetch the keys" apart
		// from "the token failed a check" — the two arrive as one error otherwise.
		select {
		case res := <-ch:
			if res.Err != nil {
				return nil, fmt.Errorf("%w: %w", ErrKeyUnavailable, res.Err)
			}
			return res.Val, nil
		case <-ctx.Done():
			// A caller-side cancellation still means no key was obtained, so no claim
			// about the token was established. That it was OUR wait that ended rather
			// than Heimdall's answer does not make the token bad.
			return nil, fmt.Errorf("%w: %w", ErrKeyUnavailable, ctx.Err())
		}
	}
}

// orDefault trims v and substitutes def when the result is empty.
func orDefault(v, def string) string {
	if t := strings.TrimSpace(v); t != "" {
		return t
	}
	return def
}

// VerifyActor validates the token and returns the actor its claims describe.
//
// Every failure that is a verdict on the TOKEN wraps ErrUnauthenticated, undifferentiated
// on purpose; the reason is for the caller to LOG, never send. The one failure that is not
// such a verdict — the signing keys could not be retrieved — wraps ErrKeyUnavailable
// instead, so the caller can report this service's outage as one rather than as the
// caller's fault.
func (v *Verifier) VerifyActor(ctx context.Context, token string) (*model.Actor, error) {
	if v.mock != nil {
		a := *v.mock
		return &a, nil
	}
	if v.validator == nil {
		// Unreachable through New; a zero Verifier must still not authenticate.
		return nil, fmt.Errorf("%w: verifier is not configured", ErrUnauthenticated)
	}

	raw := strings.TrimSpace(token)
	if len(raw) >= len(bearerPrefix) && strings.EqualFold(raw[:len(bearerPrefix)], bearerPrefix) {
		raw = strings.TrimSpace(raw[len(bearerPrefix):])
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: token is empty", ErrUnauthenticated)
	}

	parsed, err := v.validator.ValidateToken(ctx, raw)
	if err != nil {
		// ValidateToken answers two different questions with one error. Most of what it
		// returns is a verdict ON THE TOKEN; a key-func failure is not — it is this
		// service unable to check anything. Collapsing the second into ErrUnauthenticated
		// makes a Heimdall outage present as "your token is invalid" with a 400 to every
		// caller holding a good one, and 400 tells them not to retry a condition that
		// clears by itself. coalesceKeyFunc tags it so the two are separable here.
		if errors.Is(err, ErrKeyUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	claims, ok := parsed.(*validator.ValidatedClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims type %T", ErrUnauthenticated, parsed)
	}
	custom, ok := claims.CustomClaims.(*heimdallClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected custom claims type %T", ErrUnauthenticated, claims.CustomClaims)
	}

	// The library validates exp only when the claim is PRESENT (validator.go:167), so a
	// correctly signed token that simply omits it would never stop working. A credential
	// with no lifetime is the one this service must not honour.
	if claims.RegisteredClaims.Expiry == 0 {
		return nil, fmt.Errorf("%w: token has no expiry", ErrUnauthenticated)
	}

	return &model.Actor{
		Username: strings.TrimSpace(custom.Principal),
		Email:    strings.TrimSpace(custom.Email),
		Name:     strings.TrimSpace(custom.Name),
	}, nil
}
