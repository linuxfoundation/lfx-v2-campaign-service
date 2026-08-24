// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ErrUnsupportedWindow is returned for a model.MetricsWindow this client does not map to a
// LinkedIn Ad Analytics date range (currently: yesterday, last_14_days).
var ErrUnsupportedWindow = errors.New("unsupported metrics window")

// redactHTTPDoError redacts an error from httpClient.Do, which may carry full URLs with bearer
// tokens in a *url.Error wrapper. Preserves classification sentinels and pre-send dial errors
// so callers can still distinguish retryable vs permanent failures via errors.Is/errors.As,
// but returns the canonical sentinel (not the *url.Error wrapper) for context errors, a
// *dialError with a fixed Error() for pre-send dial errors, or a safe generic message for
// mid-flight/RoundTripper errors. No untrusted error strings survive.
//
// http.Client.Do returns a *url.Error when the round-trip fails. If it's a pre-send dial
// error (DNS, ECONNREFUSED), the *url.Error wraps the inner error but includes the full
// URL. isPreSendDialError traverses the wrapper via errors.As/errors.Is, but err.Error()
// still renders the URL verbatim. The dial error is therefore rebuilt from its
// classification by safeDialCause and hidden behind dialError, which keeps it matchable by
// errors.Is/errors.As but renders a fixed string — and, because the cause is reconstructed
// rather than forwarded, leaves nothing of the original reachable through the chain either.
// For context cancellation/deadline, return the canonical sentinel so the wrapper is discarded.
func redactHTTPDoError(err error) error {
	// Preserve classification sentinels: return the canonical sentinel, not the wrapper.
	// errors.Is still matches the sentinel inside the wrapper, but wrapper.Error() renders
	// the URL. Return the canonical value so the wrapper and its URL are discarded.
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// If this is a pre-send dial error (DNSError, ECONNREFUSED, etc.), keep it classifiable
	// but return a chain built entirely from values this package owns.
	if isPreSendDialError(err) {
		// Do NOT carry the original cause forward, not even after stripping *url.Error
		// layers: the layer that renders text is not necessarily a *url.Error. A custom
		// RoundTripper (WithHTTPClient takes an arbitrary one) can return
		// fmt.Errorf("Bearer <token>: %w", dnsErr), and http.Client.Do wraps THAT in a
		// *url.Error — stripping the outer layer leaves the credential-bearing
		// *fmt.wrapError as the cause, still reachable by anything that renders or
		// inspects the chain. Rebuild the cause from the classification alone instead.
		return &dialError{cause: safeDialCause(err)}
	}
	// Mid-flight or RoundTripper error: no classification possible, return safe generic message.
	return fmt.Errorf("analytics request failed")
}

// errDialFailed is the default-deny cause for a pre-send dial failure whose classification
// this package does not recognize. It carries no text from the original error.
var errDialFailed = errors.New("connection error")

// safeDialCause maps an untrusted dial error onto a value THIS package constructs, so no
// field of the original — message text, DNSError.Name, DNSError.Server — survives into the
// returned chain while errors.Is/errors.As classification is preserved. It is default-deny:
// only the classifications isPreSendDialError itself recognizes are reproduced, and anything
// else collapses to errDialFailed rather than being passed through. Mirrors the safe-cause
// mapping in internal/platform/hubspot/client.go.
func safeDialCause(err error) error {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return syscall.ECONNREFUSED
	case errors.Is(err, syscall.EHOSTUNREACH):
		return syscall.EHOSTUNREACH
	case errors.Is(err, syscall.ENETUNREACH):
		return syscall.ENETUNREACH
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Rebuild from the boolean classification bits only. Err/Name/Server are all
		// rendered by (*net.DNSError).Error() and are exactly the fields an untrusted
		// transport would use to smuggle a URL or a credential out.
		return &net.DNSError{
			Err:         "dns lookup failed",
			IsNotFound:  dnsErr.IsNotFound,
			IsTemporary: dnsErr.IsTemporary,
			IsTimeout:   dnsErr.IsTimeout,
		}
	}
	return errDialFailed
}

// dialError is the redacted form of a pre-send dial failure. Error() is a fixed string, so
// nothing from the wrapped cause is ever rendered into a log line; Unwrap keeps the cause
// reachable via errors.Is/errors.As so callers can still classify the failure (DNS, refused,
// timeout) and decide whether to retry.
//
// Unwrapping the *url.Error layers alone is not enough: WithHTTPClient accepts an arbitrary
// RoundTripper, so the innermost error is not this package's to trust — a custom transport can
// return a dial-shaped error whose text, or whose net.DNSError.Name, carries a URL or a
// credential, and the layer holding that text need not be a *url.Error. transportError.Err is
// exported and reaches error-level logging, so the fixed string is what must be exported, not
// the cause — and cause itself is always a safeDialCause reconstruction, never the original.
type dialError struct {
	cause error
}

// Error returns a fixed description that never includes text from the wrapped cause.
func (e *dialError) Error() string { return "analytics request failed: connection error" }

// Unwrap exposes the cause to errors.Is/errors.As only — never to Error()-based rendering.
func (e *dialError) Unwrap() error { return e.cause }

// redactBodyReadError redacts an error from buf.ReadFrom (response body I/O). Body-read errors
// are local I/O failures after the connection is established, carrying no credentials or URLs,
// but the actual error may include details a malicious RoundTripper or local I/O path injected.
// Returns the canonical sentinel for context errors or a fixed, distinct message for I/O errors,
// so callers can distinguish body-read failures from round-trip failures and still detect
// cancellation/deadline via errors.Is.
func redactBodyReadError(err error) error {
	// Body reads can only fail due to local I/O or response frame issues, never credentials,
	// but return the canonical sentinels for context cancellation/deadline so the caller
	// can still detect them via errors.Is.
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("read response body failed")
}

// AdAnalyticsElement is one analytics record returned by the Ad Analytics API.
// The API aggregates metrics for the requested campaign over the given date range.
type AdAnalyticsElement struct {
	// Impressions is the number of times the ad was displayed.
	Impressions int64 `json:"impressions"`
	// Clicks is the number of times the ad was clicked.
	Clicks int64 `json:"clicks"`
	// CostInUsd is the spend in decimal USD (e.g. 25.50 for $25.50). LinkedIn's
	// Ad Analytics API returns this as a JSON string representing a BigDecimal,
	// parsed after decode by costInUsdToMicros into int64 micros. This is always USD,
	// regardless of the ad account's billing currency configuration.
	CostInUsd *string `json:"costInUsd"`
	// ExternalWebsiteConversions is the total number of times users took a desired action
	// after clicking on or seeing the ad, per LinkedIn's Ads Reporting metrics schema, which
	// types it `long` with default "0". It covers both post-click and post-view conversions
	// (the schema also exposes those separately as externalWebsitePostClickConversions and
	// externalWebsitePostViewConversions); the combined figure is the one that answers
	// "did this campaign convert at all".
	//
	// A POINTER because LinkedIn returns ONLY impressions and clicks unless a metric is
	// named in the request's `fields` list. That makes an absent value ambiguous in a way
	// int64 cannot express: it means either "this campaign converted nothing" or "the field
	// was not requested / not returned", and reporting the second as 0 would flag a
	// converting campaign as dead. Nil propagates that uncertainty to the caller instead.
	ExternalWebsiteConversions *int64 `json:"externalWebsiteConversions"`
}

// AdAnalyticsResponse is the JSON response from LinkedIn's Ad Analytics endpoint.
// Elements is a pointer so a missing/null "elements" field (malformed response,
// e.g. an empty body, "{}", or "null") is distinguishable from an explicit empty
// array (genuinely zero activity in the window) — nil means "we couldn't confirm
// this is real zero-activity data" and is treated as a decode error, not silently
// reported as zero metrics.
type AdAnalyticsResponse struct {
	Elements *[]AdAnalyticsElement `json:"elements"`
}

// GetCampaignMetrics reads live campaign metrics from LinkedIn's Ad Analytics API
// for the given campaign during the specified time window. This is the platform-client
// helper behind dispatch.LinkedInDispatcher.ReadMetrics — the dispatcher, not this
// method, is what satisfies service.MetricsReader (the optional capability the
// orchestrator type-asserts for per dispatcher); the two signatures differ.
//
// campaignID is the bare numeric LinkedIn campaign id as persisted by
// campaignFromLinkedIn (trailingID of the campaign URN returned on creation)
// — NOT a URN. This method builds the sponsoredCampaign/sponsoredAccount URNs the
// Ad Analytics finder requires.
//
// The returned CampaignMetrics contains:
//   - Impressions: number of times the ad was displayed
//   - Clicks: number of times the ad was clicked
//   - CostMicros: spend in micros of USD (the Ad Analytics API returns costInUsd
//     which is always in USD regardless of the ad account's billing currency
//     configuration)
//   - Ctr: clicks/impressions (0 when impressions is 0)
func (c *Client) GetCampaignMetrics(ctx context.Context, accountID, campaignID string, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	if campaignID == "" {
		return nil, fmt.Errorf("campaign ID is required")
	}
	if accountID == "" {
		return nil, fmt.Errorf("account ID is required")
	}

	startDate, endDate, err := c.dateRangeForWindow(window)
	if err != nil {
		return nil, err
	}

	resp, err := c.makeAdAnalyticsRequest(ctx, accountID, campaignID, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("get campaign metrics: %w", err)
	}

	// No elements is not an error: the finder returns an empty array (never a
	// null/missing "elements" field on a well-formed 2xx — that case is rejected
	// as a decode error inside makeAdAnalyticsRequest) when the campaign had no
	// activity in the window.
	//
	// A no-activity window is a MEASUREMENT, not an absence of one, so Conversions is a
	// non-nil zero here — the same answer googleads.GetCampaignMetrics gives for its
	// equivalent no-rows branch. The request named externalWebsiteConversions in `fields`
	// and LinkedIn answered with a well-formed empty array, so the metric WAS asked for and
	// the campaign simply did nothing in the window. Leaving it nil would report "LinkedIn
	// cannot measure conversions", which per model.CampaignMetrics.Conversions is reserved
	// for platforms that cannot report a campaign-level count AT ALL — a claim that is false
	// for LinkedIn and that this branch has no evidence for.
	//
	// This is distinct from the per-element omission handled in the aggregation loop below,
	// which withdraws the total to nil: THERE an element LinkedIn returned came back without
	// the metric, which is missing data about activity that happened. HERE there is no
	// activity to have measured. Impressions/Clicks/Cost already answer 0 as measurements on
	// this branch; the pointer now agrees with them rather than contradicting them in the
	// same struct.
	//
	// No effect on the no_conversions rule either way: it is gated on
	// Clicks >= minClicksForConversions (internal/service/rules/actions.go) and this branch
	// reports zero clicks, so the rule cannot fire on it. What the zero buys is that every
	// other consumer — the brief response, a conversion total — reads "measured, and the
	// answer was none" instead of "unmeasured".
	if len(*resp.Elements) == 0 {
		zero := 0.0
		return &model.CampaignMetrics{CampaignID: campaignID, Window: window, Conversions: &zero}, nil
	}

	// Aggregate metrics from all elements. In practice there should be one element
	// for a single campaign over a single (non-daily) date range.
	var metrics model.CampaignMetrics
	// Conversions accumulate in an int64 for the whole loop and are widened to the
	// response's float64 exactly once, after aggregation — the same shape CostMicros uses
	// above. Carrying the running total in the float64 field itself would convert it back
	// and forth on every element, and above 2^53 float64 cannot represent every consecutive
	// integer, so each round trip can silently drop increments (and feed the overflow check
	// below a value that is no longer the true sum).
	//
	// Two flags, not one, because presence and completeness are different questions.
	// conversionsMeasured records that at least one element CARRIED the metric;
	// conversionsIncomplete records that at least one element OMITTED it. Accumulating
	// outside metrics.Conversions is what lets an omission found on a LATER element still
	// withdraw a total the earlier ones already contributed to — writing straight to the
	// response field would leave that partial sum published.
	var conversions int64
	var conversionsMeasured bool
	conversionsIncomplete := false
	for _, elem := range *resp.Elements {
		// Reject negative counts and check the running sum before adding, the same
		// guard costInUsd's aggregation applies below: two individually valid int64
		// counts can still overflow the sum, and a negative element would silently
		// understate it instead of failing the read.
		if elem.Impressions < 0 || elem.Clicks < 0 {
			return nil, fmt.Errorf("get campaign metrics: negative impressions or clicks in response element")
		}
		// Clicks without impressions is not a low number, it is an impossible one: every
		// click is preceded by the impression that carried it. Reaching here means the
		// element is incomplete (a metric key omitted from the JSON decodes to the zero
		// value, indistinguishable from a real zero) and reporting it would publish
		// Ctr=0 alongside a non-zero click count — a plausible-looking figure derived
		// from data we could not confirm.
		//
		// Presence tracking (*int64 per metric) is deliberately NOT the fix here: we
		// cannot tell an omitted-because-zero field from an omitted-because-malformed
		// one, so requiring every key would reject responses that are genuinely fine.
		// This guard keys on the one relationship that stays decidable either way.
		if elem.Clicks > 0 && elem.Impressions == 0 {
			return nil, fmt.Errorf("get campaign metrics: response element reports clicks with zero impressions")
		}
		if elem.Impressions > math.MaxInt64-metrics.Impressions {
			return nil, fmt.Errorf("get campaign metrics: aggregate impressions overflows int64")
		}
		if elem.Clicks > math.MaxInt64-metrics.Clicks {
			return nil, fmt.Errorf("get campaign metrics: aggregate clicks overflows int64")
		}
		metrics.Impressions += elem.Impressions
		metrics.Clicks += elem.Clicks
		// Conversions are published only when EVERY element carried the metric: one element
		// omitting externalWebsiteConversions withdraws the whole total to nil, and so does a
		// response where none carried it. Treating an absent value as a zero addend would
		// silently convert "LinkedIn did not report this" into a measured zero — the
		// substitution the pointer exists to prevent — and it would do so most often on
		// exactly the responses where the field was never requested.
		if elem.ExternalWebsiteConversions == nil {
			// An OMITTED metric poisons the whole total rather than being skipped over.
			//
			// The alternative — sum the elements that do carry a value and report that — is
			// wrong in a way the type cannot express: it hands every consumer a PARTIAL count
			// labelled as a complete measurement. Consider two elements where the first omits
			// the field and the second reports an explicit 0. Summing the present ones yields
			// exactly 0, while the clicks from BOTH elements still aggregate — so once they
			// clear minClicksForConversions the no_conversions rule fires HIGH against a
			// campaign whose real conversion count is simply unknown. That is the rule
			// manufacturing its own finding rather than measuring one.
			//
			// nil already means precisely "LinkedIn did not report this", which is the honest
			// answer for a partially-covered response, and model.CampaignMetrics.Conversions
			// documents that the rule refuses to fire on nil. This mirrors the convIncomplete
			// discipline in microsoft/metrics.go, where one blank ConversionsQualified cell
			// withdraws that report's entire total, for the same reason and against the same
			// rule.
			conversionsIncomplete = true
		} else {
			conv := *elem.ExternalWebsiteConversions
			// Same guards the counters above get, and for the same reason: a negative count
			// is malformed upstream data rather than a small number, and two valid int64
			// values can still overflow their sum. Either would otherwise become a figure the
			// conversions rule reads as a measurement.
			if conv < 0 {
				return nil, fmt.Errorf("get campaign metrics: negative externalWebsiteConversions in response element")
			}
			// externalWebsiteConversions is typed `long` in LinkedIn's Ads Reporting schema —
			// unlike Google's and Microsoft's doubles, it carries no fraction — so the exact
			// integer overflow guard runs against the int64 accumulator, where it stays exact.
			if conv > math.MaxInt64-conversions {
				return nil, fmt.Errorf("get campaign metrics: aggregate externalWebsiteConversions overflows int64")
			}
			conversions += conv
			conversionsMeasured = true
		}
		if elem.CostInUsd != nil {
			// LinkedIn's Ad Analytics API returns costInUsd as a BigDecimal serialized as a
			// JSON string. A present but malformed/non-finite/negative/overflowing value
			// must fail the read, not be silently dropped: dropping it returns 200 with
			// understated spend while impressions/clicks still report normally, which looks
			// like a real (if low) cost rather than a decode failure.
			micros, err := costInUsdToMicros(*elem.CostInUsd)
			if err != nil {
				// The raw costInUsd value is deliberately NOT interpolated here. This error
				// propagates to BriefService.GetCampaignMetrics's default branch, which logs it,
				// so echoing unvalidated response content into the message would put it in the
				// server log -- the same pattern 1db44ee removed from the adAnalytics apiError
				// on this branch. The sibling count guards below are already value-free.
				return nil, fmt.Errorf("get campaign metrics: parse costInUsd: %w", err)
			}
			// Each micros value individually fits int64 (costInUsdToMicros rejects an
			// overflowing single value), but the running SUM across elements can still
			// overflow — reject rather than silently wrap into a negative CostMicros.
			if micros > math.MaxInt64-metrics.CostMicros {
				return nil, fmt.Errorf("get campaign metrics: aggregate costInUsd overflows int64 micros")
			}
			metrics.CostMicros += micros
		}
	}

	// Widen the aggregated count once, and only if EVERY element carried the metric. One
	// element omitting it anywhere in the response leaves Conversions nil — see the reasoning
	// at the accumulation site. A nil Conversions means LinkedIn never reported it, or
	// reported it incompletely; both stay distinct from a measured zero.
	if conversionsMeasured && !conversionsIncomplete {
		total := float64(conversions)
		metrics.Conversions = &total
	}

	// Calculate Ctr: clicks / impressions. If impressions is 0, Ctr stays 0.
	if metrics.Impressions > 0 {
		metrics.Ctr = float64(metrics.Clicks) / float64(metrics.Impressions)
	}

	metrics.CampaignID = campaignID
	metrics.Window = window

	return &metrics, nil
}

// costInUsdToMicros parses a LinkedIn Ad Analytics costInUsd decimal string (e.g.
// "25.50") into micros of USD, rounding rather than truncating so a value like
// 25.5000005 doesn't silently lose a micro (truncation gives 25_500_000, rounding
// gives 25_500_001 — a value with six or fewer fractional digits, such as 25.505,
// converts exactly and cannot distinguish the two). It rejects anything that isn't a clean,
// finite, non-negative decimal, or that would overflow int64 once converted to
// micros, so a malformed value surfaces as a decode error instead of being
// silently coerced into an understated cost.
//
// Parsed via big.Rat, not strconv.ParseFloat: costInUsd is a BigDecimal on
// LinkedIn's side, and float64 only has ~15-17 significant decimal digits of
// precision. At magnitudes near math.MaxInt64 micros (~9.2e12 USD), float64's
// representable-value spacing exceeds 1 micro, so a float64 intermediate can
// silently misrepresent the exact value before the overflow check even runs.
// big.Rat represents the decimal exactly, so the rounding below is the only
// place precision is intentionally lost — to the nearest whole micro.
// decimalCostPattern matches a plain, optionally-negative decimal number: no
// slashes, exponents, or other big.Rat-only rational syntax (e.g. "1/2"),
// which SetString would otherwise accept and silently misinterpret as a
// fraction rather than reject as malformed.
var decimalCostPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// supportedMetricsWindows is exactly the set dateRangeForWindow maps to an Ad Analytics
// date range. It is package-level and clock-free precisely so a caller can decide
// "LinkedIn cannot serve this window" without a Client, credentials, or a network call.
var supportedMetricsWindows = map[model.MetricsWindow]struct{}{
	model.MetricsWindowToday:      {},
	model.MetricsWindowLast7Days:  {},
	model.MetricsWindowLast30Days: {},
	model.MetricsWindowThisMonth:  {},
	model.MetricsWindowLastMonth:  {},
}

// ValidateMetricsWindow reports whether this client can map window to an Ad Analytics
// date range, returning ErrUnsupportedWindow if it cannot.
//
// It exists so the dispatcher can reject an unsupported window BEFORE resolving
// credentials. Order matters for the status code the caller sees: an unsupported window
// is a permanent 400 no matter what state the connection is in, but if credential
// resolution runs first, a project whose connection is inactive or incomplete fails with
// a connection error that BriefService maps to 503 — telling the caller to retry a
// request that can never succeed. The X adapter already validates in this order
// (internal/dispatch/twitter.go).
func ValidateMetricsWindow(window model.MetricsWindow) error {
	if _, ok := supportedMetricsWindows[window]; !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}
	return nil
}

// None of the failure paths below interpolate s. Every one of them is reached
// only for a value that FAILED validation, and the error propagates up through
// GetCampaignMetrics to BriefService.GetCampaignMetrics's default branch, which
// logs it -- so echoing the value would write unvalidated upstream response
// content to the server log. The distinct messages plus the value's length are
// enough to diagnose a malformed response without reproducing its bytes.
// maxCostDecimalLen bounds the costInUsd string before any big.Rat work. The 10 MiB
// response cap is not a bound on this: a single well-formed decimal can fill it, and
// while decimalCostPattern's scan is linear, big.Rat.SetString, the 1e6 multiply, and
// FloatString(0) are super-linear in digit count AND do not observe the request
// context, so they keep burning CPU after the 20s call deadline has passed. 40 bytes
// is far above any real spend figure — int64 micros tops out at ~9.2e12 USD, 13
// integer digits — while leaving no room for an adversarial one.
const maxCostDecimalLen = 40

func costInUsdToMicros(s string) (int64, error) {
	if len(s) > maxCostDecimalLen {
		return 0, fmt.Errorf("decimal exceeds %d bytes (%d bytes)", maxCostDecimalLen, len(s))
	}
	if !decimalCostPattern.MatchString(s) {
		return 0, fmt.Errorf("not a valid decimal (%d bytes)", len(s))
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("decimal did not parse (%d bytes)", len(s))
	}
	if r.Sign() < 0 {
		return 0, fmt.Errorf("negative value (%d bytes)", len(s))
	}
	scaled := new(big.Rat).Mul(r, big.NewRat(1_000_000, 1))
	// FloatString(0) rounds to the nearest integer (ties away from zero), giving
	// the same "round rather than truncate" behavior as the prior float64 path,
	// but without the intermediate precision loss.
	micros, ok := new(big.Int).SetString(scaled.FloatString(0), 10)
	if !ok || !micros.IsInt64() {
		return 0, fmt.Errorf("value overflows int64 micros (%d bytes)", len(s))
	}
	return micros.Int64(), nil
}

// restLiDate renders a time.Time as a Rest.li 2.0 nested date object literal,
// e.g. "(day:15,month:1,year:2025)".
func restLiDate(t time.Time) string {
	return fmt.Sprintf("(day:%d,month:%d,year:%d)", t.Day(), int(t.Month()), t.Year())
}

// makeAdAnalyticsRequest makes a raw HTTP request to LinkedIn's Ad Analytics
// finder and parses the response into AdAnalyticsResponse.
//
// This bypasses doRequest (which unmarshals into the campaign/creative-shaped
// linkedInResponse) because Ad Analytics responses have a different JSON shape,
// AND because the Ad Analytics finder uses Rest.li 2.0 array/nested-object query
// parameter syntax (List(...), nested dateRange) that doRequest's flat
// map[string]string params can't express. It reuses the client's 429 retry
// policy (parseRetryAfter/retryBaseDelay/maxRetryWait) so an analytics read is
// retried the same way doRequest retries idempotent GETs — see
// docs/knowledge/code/internal-platform-linkedin.md.
//
// UNVERIFIED ASSUMPTION: the finder name (q=analytics), pivot=CAMPAIGN, and
// timeGranularity=ALL are LinkedIn's documented Ad Analytics contract, but this
// has not yet been verified against a live LinkedIn Marketing API account.
func (c *Client) makeAdAnalyticsRequest(ctx context.Context, accountID, campaignID string, startDate, endDate time.Time) (*AdAnalyticsResponse, error) {
	campaignURN := "urn:li:sponsoredCampaign:" + campaignID
	accountURN := "urn:li:sponsoredAccount:" + accountID

	u, err := url.Parse(c.baseURL + "/" + "adAnalytics")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	// Rest.li 2.0 List()/nested-object literals are NOT standard query-string
	// values, so they are appended to RawQuery directly rather than through
	// url.Values.Encode() (which would percent-encode the structural
	// parentheses/colons LinkedIn's finder requires literally).
	rawQuery := "q=analytics" +
		"&pivot=CAMPAIGN" +
		"&timeGranularity=ALL" +
		"&dateRange=(start:" + restLiDate(startDate) + ",end:" + restLiDate(endDate) + ")" +
		"&campaigns=List(" + url.QueryEscape(campaignURN) + ")" +
		"&accounts=List(" + url.QueryEscape(accountURN) + ")" +
		// externalWebsiteConversions must be named explicitly: LinkedIn's Ad Analytics finder
		// returns only impressions and clicks when `fields` is omitted, and omitting a metric
		// from this list makes it absent from the response rather than zero. The finder
		// accepts up to 20 metrics, so this is well inside the limit.
		"&fields=impressions,clicks,costInUsd,externalWebsiteConversions"
	u.RawQuery = rawQuery

	idempotent := true // GET is always retried on 429, same as doRequest's SAFE-method rule.
	for attempt := 0; attempt <= retryMax; attempt++ {
		resp, retry, wait, err := c.doAdAnalyticsAttempt(ctx, u.String())
		if err != nil {
			return nil, err
		}
		if retry {
			if attempt < retryMax && idempotent {
				if wait <= 0 {
					wait = c.retryBaseDelay * time.Duration(1<<uint(attempt))
				}
				if wait > maxRetryWait {
					wait = maxRetryWait
				}
				if err := sleepCtx(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
			// Retries exhausted on the last attempt: falling through here would return
			// (nil, nil) — a silent "success" with no data that panics on the caller's
			// *resp.Elements dereference. Surface the exhaustion as a terminal error
			// instead, same shape doRequest's inline 429 handling produces.
			return nil, &apiError{StatusCode: http.StatusTooManyRequests, Method: "GET", Path: "adAnalytics", Body: "rate limited: retries exhausted"}
		}
		return resp, nil
	}
	panic("linkedin makeAdAnalyticsRequest: unreachable post-loop return")
}

// doAdAnalyticsAttempt performs a single Ad Analytics GET attempt. It returns
// (resp, false, 0, nil) on success, (nil, true, wait, nil) when the caller
// should retry after wait, or (nil, false, 0, err) on a terminal error.
func (c *Client) doAdAnalyticsAttempt(ctx context.Context, rawURL string) (*AdAnalyticsResponse, bool, time.Duration, error) {
	// Resolve through authorizedAttempt, NOT c.creds.AccessToken: this is the metrics
	// read path — the one that surfaced the expired-token 500 — so it must take the
	// same refresh and fail-closed discipline as doRequest. Reading the field directly
	// would send a known-expired token and would never see a refreshed one.
	//
	// It is NOT a data race: c.creds is injected at construction and never written
	// afterwards — a rotated refresh token is adopted into c.refreshToken/c.refreshExpiry
	// precisely so c.creds stays immutable (see fetchToken in token.go). The reason to go
	// through the token accessor is correctness of the VALUE, not memory safety.
	//
	// authorizedAttempt also fixes the ORDER: the refresh runs on the parent ctx under
	// its own bound, so a refresh that succeeds near that bound still leaves this Ad
	// Analytics request a full per-attempt budget.
	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	if err != nil {
		cancel()
		return nil, false, 0, err
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, false, 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		// Redact URLs/credentials: if this is a dial error wrapped in *url.Error, unwrap it
		// so callers can still classify via errors.Is/As. Mid-flight errors get a safe message.
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := c.parseRetryAfter(resp)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil, true, wait, nil
	}

	// Read the response body.
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes+1)); err != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: redactBodyReadError(err)}
		}
		// Same redaction technique as the 2xx path (redactBodyReadError): a body-read error can
		// carry untrusted upstream content regardless of status code. The two CARRIER FIELDS
		// differ — transportError.Err is surfaced as a bare cause, apiError.Body is a free-form
		// diagnostic field — but both carry redactBodyReadError's message VERBATIM, so no extra
		// prefix is needed on either. Do not add a "read response body: " prefix here: it would
		// double-state what redactBodyReadError already says, and it would also make the two
		// paths' text diverge for no reason (metrics_test.go's wantBody asserts they match).
		//
		// A 401 is classified here for the same reason the readable-body arm below does it: an
		// unreadable body does not make the 401 any less a 401. The AMBIGUITY half is a no-op on
		// this path (the method is a hard-coded GET, which the method gate keeps definite — an
		// analytics read creates nothing), but the CACHE-INVALIDATION half is not: without this,
		// a 401 whose body could not be read left the rejected token in cache to be replayed,
		// and the operator got an opaque upstream error instead of the reconnect signal.
		if ce := c.expiredCredentialsError(resp.StatusCode, "", http.MethodGet); ce != nil {
			return nil, false, 0, ce
		}
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: redactBodyReadError(err).Error()}
	}

	if int64(buf.Len()) > maxResponseBytes {
		// The cap+1 read stopped the moment the body went over, so the remainder is still
		// on the wire. Closing on an unread body makes net/http tear the connection down
		// instead of returning it to the idle pool, so every later metrics read pays a
		// fresh TCP+TLS handshake — the same reason the 429 path above discards. Bounded
		// by maxResponseBytes for the same reason the read was: an unbounded discard would
		// let an adversarial or runaway response hold the goroutine indefinitely.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
		}
		// Same reasoning as the read-failure arm above: an oversized body does not un-401 a
		// 401, and the rejected token must still be evicted from the cache.
		if ce := c.expiredCredentialsError(resp.StatusCode, "", http.MethodGet); ce != nil {
			return nil, false, 0, ce
		}
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics", Body: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A 401 is classified HERE, not left to the caller, and this is the path the
		// incident actually ran through. Resolving the token through accessTokenValue
		// above covers a token this client KNOWS is expired; it cannot cover the two
		// cases that produced the outage — a bearer-only row whose AccessTokenExpiresAt
		// is zero (so nothing predicts the expiry) and a token LinkedIn revoked
		// mid-flight. In both, the request is sent and LinkedIn answers 401, and
		// returning a bare apiError here made GetCampaignMetrics fail with an opaque
		// upstream error that the dispatcher's ErrCredentialsExpired re-tag could never
		// match. Mirrors doRequest's 401 arm in client.go.
		//
		// The body is passed to the classifier but still NOT retained on apiError: it is
		// read for the serviceErrorCode signal and discarded, so no untrusted upstream
		// text reaches an exported field.
		// expiredCredentialsError clears the cached token (so the next read re-exchanges
		// rather than replaying one LinkedIn has already rejected — only the CACHE, since a
		// revoked access token does not imply a revoked refresh token) and records
		// Method/StatusCode for the same reason doRequest's 401 arm does. Here they classify
		// NEGATIVELY and that is the point: this analytics call is a GET, so it created
		// nothing, and the method gate in createOutcomeAmbiguous keeps a read 401 a plain
		// expiry rather than letting it read as "a campaign may exist".
		if ce := c.expiredCredentialsError(resp.StatusCode, buf.String(), http.MethodGet); ce != nil {
			return nil, false, 0, ce
		}
		// Body is deliberately NOT retained: this analytics path never classifies on
		// the response body (unlike isInReviewPauseRejection's write-path use of
		// apiError.Body above), so there's no reason to hold an untrusted,
		// possibly credential-bearing response body on an exported struct field,
		// where it risks exposure via reflection/JSON-based logging even though
		// apiError.Error() itself omits it.
		return nil, false, 0, &apiError{StatusCode: resp.StatusCode, Method: "GET", Path: "adAnalytics"}
	}

	var analytics AdAnalyticsResponse
	if err := json.Unmarshal(buf.Bytes(), &analytics); err != nil {
		// json.UnmarshalTypeError.Value and json.SyntaxError can contain fragments of the
		// response body, which reaches the server log through BriefService.GetCampaignMetrics's
		// default error branch. The buffer is up to 10 MiB of unvalidated upstream content.
		// Return a safe message with byte length, not the cause, matching costInUsdToMicros.
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode response: malformed JSON (%d bytes)", len(buf.Bytes()))}
	}
	if analytics.Elements == nil {
		return nil, false, 0, &transportError{Method: "GET", Path: "adAnalytics", Err: fmt.Errorf("decode response: missing or null \"elements\" field")}
	}

	return &analytics, false, 0, nil
}

// dateRangeForWindow computes the start and end dates for the given metrics window,
// relative to the current time (c.now()). Each window is computed as:
//   - today: today
//   - last_7_days: 6 days ago through today (7 days inclusive)
//   - last_30_days: 29 days ago through today (30 days inclusive)
//   - this_month: the 1st of this month through today
//   - last_month: the 1st through the last day of the PREVIOUS calendar month
//
// Both boundaries are derived from the first-of-month anchor (never via
// AddDate(0, -1, 0) on today's day-of-month), since time.AddDate normalizes an
// invalid day-of-month (e.g. subtracting a month from the 31st) into the
// following month rather than erroring — that would silently shift both
// this_month and last_month's boundaries on 29th/30th/31st-of-month days.
//
// The window is anchored to the UTC calendar date, not the client's local one: now() is
// converted with .UTC() before year/month/day are extracted, so every caller gets the same
// range regardless of the clock's zone. This means a local date and the queried date can
// legitimately differ — a client in Asia/Tokyo (UTC+9) at 06:00 on Jan 15 local time is at
// 21:00 on Jan 14 UTC, so "today" queries Jan 14, not Jan 15. That is the intended contract,
// since LinkedIn's Ad Analytics dateRange is itself a UTC calendar range; interpreting it in
// local time would silently shift every window by a day for non-UTC callers.
func (c *Client) dateRangeForWindow(window model.MetricsWindow) (start, end time.Time, err error) {
	// Checked first so this method and ValidateMetricsWindow can never disagree about
	// which windows are supported — TestValidateMetricsWindowMatchesDateRangeForWindow
	// pins that they agree for every model.MetricsWindow.
	if err := ValidateMetricsWindow(window); err != nil {
		return time.Time{}, time.Time{}, err
	}
	now := c.now().UTC()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	firstOfThisMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	switch window {
	case model.MetricsWindowToday:
		start, end = today, today

	case model.MetricsWindowLast7Days:
		start = today.AddDate(0, 0, -6) // 6 days before today = 7 days inclusive
		end = today

	case model.MetricsWindowLast30Days:
		start = today.AddDate(0, 0, -29) // 29 days before today = 30 days inclusive
		end = today

	case model.MetricsWindowThisMonth:
		start = firstOfThisMonth
		end = today

	case model.MetricsWindowLastMonth:
		// One day before the first of this month is always the last day of the
		// previous month, regardless of how many days that month has.
		lastDayOfLastMonth := firstOfThisMonth.AddDate(0, 0, -1)
		start = time.Date(lastDayOfLastMonth.Year(), lastDayOfLastMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = lastDayOfLastMonth

	default:
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q", ErrUnsupportedWindow, window)
	}

	return start, end, nil
}
