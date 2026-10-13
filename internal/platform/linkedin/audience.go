// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Audience dimension tokens: this broker's vocabulary, one per LinkedIn Ad Analytics
// professional-demographic pivot (audiencePivots below).
const (
	AudienceDimensionJobFunction = "job_function"
	AudienceDimensionSeniority   = "seniority"
	AudienceDimensionIndustry    = "industry"
	AudienceDimensionCompanySize = "company_size"
	AudienceDimensionCountry     = "country"
)

const (
	// MaxAudienceCampaigns bounds the campaign URNs one audience read names in its `campaigns`
	// facet. LinkedIn publishes no limit for the facet's list; this is a conservative LOCAL bound
	// on the GET URL the list is encoded into (each URN is ~45 bytes percent-encoded, and this
	// client does not implement LinkedIn's query tunneling for over-long URLs), not a documented
	// LinkedIn limit.
	MaxAudienceCampaigns = 50
	// audienceMaxElements is LinkedIn's documented response ceiling for adAnalytics ("Response is
	// limited to 15,000 elements"; the endpoint has no pagination). A response AT the ceiling
	// cannot prove it is whole, so it is refused rather than summed as a complete distribution.
	audienceMaxElements = 15000
)

// ErrAudienceScopeTooLarge marks a read refused locally because the scope exceeds
// MaxAudienceCampaigns. Permanent while the project owns that many campaigns.
var ErrAudienceScopeTooLarge = errors.New("linkedin-ads: too many campaigns to scope one audience read to")

// ErrAudienceScopeInvalid marks a read refused locally because a scope entry is not a canonical
// LinkedIn campaign id, or the scope is empty. Refused rather than dropped: a reduced scope would
// be presented as the project's whole picture, and an empty one would leave the read with no
// campaign facet at all.
var ErrAudienceScopeInvalid = errors.New("linkedin-ads: audience read scope is empty or holds an invalid campaign id")

// audienceCampaignIDRE is a canonical LinkedIn campaign id: the bare positive decimal that
// campaignFromLinkedIn persists (trailingID of urn:li:sponsoredCampaign:<id>), no leading zero,
// at most 19 digits.
var audienceCampaignIDRE = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// ValidateAudienceCampaignID reports whether id is a canonical LinkedIn campaign id this read
// may interpolate into a sponsoredCampaign URN. Never echoes id.
func ValidateAudienceCampaignID(id string) error {
	if !audienceCampaignIDRE.MatchString(id) {
		return fmt.Errorf("%w (%d bytes)", ErrAudienceScopeInvalid, len(id))
	}
	return nil
}

// audienceValueRE is the charset a pivot value must match to be returned verbatim. It admits
// every documented professional-demographic value shape (urn:li:function:8, urn:li:geo:103644278,
// …) and nothing that could carry markup, whitespace or control bytes. A value outside it is not
// dropped: the row is untrustworthy and the read fails.
var audienceValueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\-]{0,127}$`)

// campaignURNPrefix is the pivot value the CAMPAIGN pivot yields.
const campaignURNPrefix = "urn:li:sponsoredCampaign:"

// audiencePivot is one statistics request: the dimension token, LinkedIn's MEMBER_* pivot, and
// the URN prefix its values must carry ("" = LinkedIn documents no URN shape for the pivot, so
// only the charset is enforced).
type audiencePivot struct {
	dimension string
	pivot     string
	urnPrefix string
}

// audiencePivots are LinkedIn's documented professional-demographic pivots this read serves, in
// response order (ads-reporting-schema, "Analytics Finder Query Parameters"). The URN shapes are
// the ones LinkedIn's URN-resolution table names for each type. MEMBER_COUNTRY_V2 (not the
// retired MEMBER_COUNTRY) is the documented country/region pivot. MEMBER_JOB_TITLE, MEMBER_COMPANY,
// MEMBER_REGION_V2, MEMBER_COUNTY and MEMBER_DESIGNATED_MARKET_AREA are documented too and are
// deliberately not read: each is a long-tailed value space that the top-100-per-creative-per-day
// cap truncates hardest, and DMA carries a Nielsen display obligation.
var audiencePivots = []audiencePivot{
	{dimension: AudienceDimensionJobFunction, pivot: "MEMBER_JOB_FUNCTION", urnPrefix: "urn:li:function:"},
	{dimension: AudienceDimensionSeniority, pivot: "MEMBER_SENIORITY", urnPrefix: "urn:li:seniority:"},
	{dimension: AudienceDimensionIndustry, pivot: "MEMBER_INDUSTRY", urnPrefix: "urn:li:industry:"},
	{dimension: AudienceDimensionCompanySize, pivot: "MEMBER_COMPANY_SIZE", urnPrefix: ""},
	{dimension: AudienceDimensionCountry, pivot: "MEMBER_COUNTRY_V2", urnPrefix: "urn:li:geo:"},
}

// AudienceBucket is one pivot value's counters, summed across the scoped campaigns.
type AudienceBucket struct {
	Dimension string
	// Value is LinkedIn's pivot value, verbatim (a URN; NOT resolved to a name).
	Value       string
	Impressions int64
	Clicks      int64
	// CostMicros is costInUsd in micros of USD.
	CostMicros int64
	// Ctr is Clicks/Impressions computed AFTER aggregation, 0 when Impressions is 0.
	Ctr float64
}

// AudienceInsights is the audience read across every pivot, confined to the caller's campaigns.
// Every counter is APPROXIMATE: LinkedIn documents professional-demographic metrics as
// privacy-approximated and thresholded.
type AudienceInsights struct {
	Window  model.MetricsWindow
	Buckets []AudienceBucket
}

// audienceElement is one statistics element. The counters are RAW so an ABSENT key (read as 0,
// the schema's default for an unreported long) is distinguishable from an explicit JSON null,
// which a plain int64 would decode as 0 and publish as an authoritative zero for a value LinkedIn
// explicitly did not give; a null is refused instead.
type audienceElement struct {
	PivotValues *[]string       `json:"pivotValues"`
	Impressions json.RawMessage `json:"impressions"`
	Clicks      json.RawMessage `json:"clicks"`
	CostInUsd   json.RawMessage `json:"costInUsd"`
}

// GetAudienceInsights reads LinkedIn's professional demographics over window across the campaigns
// named by campaignIDs — NOT the whole ad account.
//
// Each pivot is one `GET /adAnalytics?q=statistics&pivots=List(CAMPAIGN,MEMBER_…)` with a
// `campaigns` facet of the scoped campaign URNs and NO `accounts` facet (the campaign facet is the
// tenant boundary; LinkedIn documents `campaigns` as sufficient on its own). The CAMPAIGN pivot is
// requested alongside the demographic one so the boundary is ALSO checked on the way back: every
// element must name a campaign in the scope or the read fails — once a foreign campaign's spend
// is summed into a bucket it is indistinguishable from the project's own.
//
// The window is the LinkedIn metrics read's (dateRangeForWindow: UTC calendar days); yesterday
// and last_14_days are ErrUnsupportedWindow exactly as there.
//
// All or nothing: a failure on any pivot, a malformed, foreign or duplicated element, an
// untrustworthy pivot value, an explicit-null or malformed counter, a `next` paging link, or a
// response at LinkedIn's 15,000-element ceiling fails the whole call.
//
// Single attempt per pivot, like the account monitor's analytics read: a 429 surfaces as an
// error (the caller answers 503) rather than spending the request's budget on backoff.
func (c *Client) GetAudienceInsights(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	start, end, err := c.dateRangeForWindow(window)
	if err != nil {
		return nil, fmt.Errorf("get audience insights: %w", err)
	}
	inScope, facet, err := audienceScope(campaignIDs)
	if err != nil {
		return nil, err
	}
	out := &AudienceInsights{Window: window, Buckets: []AudienceBucket{}}
	for _, p := range audiencePivots {
		buckets, perr := c.readAudiencePivot(ctx, p, start, end, facet, inScope)
		if perr != nil {
			return nil, fmt.Errorf("get audience insights (%s): %w", p.dimension, perr)
		}
		out.Buckets = append(out.Buckets, buckets...)
	}
	return out, nil
}

// audienceScope validates the campaign ids and renders the `campaigns` facet value.
// Duplicates collapse; an empty, invalid or oversized scope is refused before any request.
func audienceScope(campaignIDs []string) (map[string]struct{}, string, error) {
	if len(campaignIDs) == 0 {
		return nil, "", fmt.Errorf("get audience insights: %w", ErrAudienceScopeInvalid)
	}
	inScope := make(map[string]struct{}, len(campaignIDs))
	urns := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		if err := ValidateAudienceCampaignID(id); err != nil {
			return nil, "", fmt.Errorf("get audience insights: %w", err)
		}
		if _, dup := inScope[id]; dup {
			continue
		}
		inScope[id] = struct{}{}
		urns = append(urns, url.QueryEscape(campaignURNPrefix+id))
	}
	if len(urns) > MaxAudienceCampaigns {
		return nil, "", fmt.Errorf("get audience insights: %d campaigns (at most %d): %w", len(urns), MaxAudienceCampaigns, ErrAudienceScopeTooLarge)
	}
	return inScope, "List(" + strings.Join(urns, ",") + ")", nil
}

// readAudiencePivot issues one statistics request and aggregates its elements by pivot value.
func (c *Client) readAudiencePivot(ctx context.Context, p audiencePivot, start, end time.Time, facet string, inScope map[string]struct{}) ([]AudienceBucket, error) {
	u, err := url.Parse(c.baseURL + "/adAnalytics")
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	// Rest.li 2.0 List()/nested-object literals go into RawQuery verbatim (see
	// makeAdAnalyticsRequest). Every interpolated value is a constant, a date this client
	// computed, or a query-escaped URN of a validated id. pivotValues is not named in `fields`:
	// it is returned with the pivots, as the account monitor's CAMPAIGN-pivot read relies on.
	u.RawQuery = "q=statistics" +
		"&pivots=List(CAMPAIGN," + p.pivot + ")" +
		"&timeGranularity=ALL" +
		"&dateRange=(start:" + restLiDate(start) + ",end:" + restLiDate(end) + ")" +
		"&campaigns=" + facet +
		"&fields=impressions,clicks,costInUsd"

	body, err := c.getAdAnalyticsRaw(ctx, u.String())
	if err != nil {
		return nil, err
	}
	malformed := func(format string, args ...any) error {
		return &transportError{Method: http.MethodGet, Path: "adAnalytics", Err: fmt.Errorf(format, args...)}
	}
	// Checked on the RAW bytes before decoding: encoding/json resolves a repeated key (exact or
	// case-folded, at any level) in favour of the last value with no error, so
	// `{"elements":[…],"elements":[]}` would decode as an authoritative empty answer.
	if err := identityjson.Check(body); err != nil {
		return nil, malformed("audience statistics: %v", err)
	}
	var parsed struct {
		// Pointer so `{}`, `"elements":null` and a missing field are distinct from LinkedIn's
		// authoritative empty array (no activity, or no read access).
		Elements *[]json.RawMessage `json:"elements"`
		Paging   *struct {
			Links []struct {
				Rel string `json:"rel"`
			} `json:"links"`
		} `json:"paging"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// The decoder's message can quote a value; report the shape only.
		return nil, malformed("audience statistics: malformed JSON (%d bytes)", len(body))
	}
	if parsed.Elements == nil {
		return nil, malformed("audience statistics: response has no elements field")
	}
	if parsed.Paging != nil {
		for _, l := range parsed.Paging.Links {
			if strings.EqualFold(strings.TrimSpace(l.Rel), "next") {
				// adAnalytics documents no pagination; a `next` link says this answer is not
				// whole, and summing part of a distribution looks exactly like all of it.
				return nil, malformed("audience statistics: response offers a next page; refusing a partial distribution")
			}
		}
	}
	if len(*parsed.Elements) >= audienceMaxElements {
		return nil, malformed("audience statistics: %d elements reaches LinkedIn's %d-element response ceiling; cannot prove the answer is whole", len(*parsed.Elements), audienceMaxElements)
	}

	totals := map[string]*AudienceBucket{}
	seen := map[string]struct{}{}
	for i, raw := range *parsed.Elements {
		var el audienceElement
		if err := json.Unmarshal(raw, &el); err != nil {
			return nil, malformed("audience statistics element %d: not an object of the expected shape", i)
		}
		campaignID, value, verr := audiencePivotValues(el.PivotValues, p)
		if verr != nil {
			return nil, malformed("audience statistics element %d: %v", i, verr)
		}
		if _, ok := inScope[campaignID]; !ok {
			// Not echoed: the id is unvalidated upstream content.
			return nil, malformed("audience statistics element %d: names a campaign outside the requested scope (%d bytes)", i, len(campaignID))
		}
		key := campaignID + "\x00" + value
		if _, dup := seen[key]; dup {
			// timeGranularity=ALL yields one element per (campaign, value); a repeat would
			// double-count when summed.
			return nil, malformed("audience statistics element %d: duplicate element for one campaign and value", i)
		}
		seen[key] = struct{}{}
		impressions, ierr := audienceCount("impressions", el.Impressions)
		clicks, cerr := audienceCount("clicks", el.Clicks)
		cost, serr := audienceCostMicros(el.CostInUsd)
		if err := errors.Join(ierr, cerr, serr); err != nil {
			return nil, malformed("audience statistics element %d: %v", i, err)
		}
		bucket, ok := totals[value]
		if !ok {
			bucket = &AudienceBucket{Dimension: p.dimension, Value: value}
			totals[value] = bucket
		}
		if err := addAudienceCounters(bucket, impressions, clicks, cost); err != nil {
			return nil, malformed("audience statistics element %d: %v", i, err)
		}
	}
	return finishAudienceBuckets(totals), nil
}

// audiencePivotValues splits an element's pivotValues into its campaign id and its demographic
// value. Exactly two values are required, exactly one of them a sponsoredCampaign URN; the order
// is not relied on. Neither value is echoed into an error.
func audiencePivotValues(values *[]string, p audiencePivot) (string, string, error) {
	if values == nil {
		return "", "", errors.New("pivotValues is missing")
	}
	if len(*values) != 2 {
		return "", "", fmt.Errorf("pivotValues carries %d values, want 2 (CAMPAIGN and %s)", len(*values), p.pivot)
	}
	var campaignID, value string
	campaigns := 0
	for _, v := range *values {
		if strings.HasPrefix(v, campaignURNPrefix) {
			campaigns++
			campaignID = strings.TrimPrefix(v, campaignURNPrefix)
			continue
		}
		value = v
	}
	if campaigns != 1 {
		return "", "", fmt.Errorf("pivotValues carries %d campaign URNs, want exactly 1", campaigns)
	}
	if !audienceValueRE.MatchString(value) {
		return "", "", fmt.Errorf("%s value is outside the accepted charset (%d bytes)", p.pivot, len(value))
	}
	if p.urnPrefix != "" && (!strings.HasPrefix(value, p.urnPrefix) || len(value) == len(p.urnPrefix)) {
		return "", "", fmt.Errorf("%s value is not a %s URN (%d bytes)", p.pivot, strings.TrimSuffix(p.urnPrefix, ":"), len(value))
	}
	return campaignID, value, nil
}

// audienceCount decodes an integer counter: absent is 0 (the schema's default for an
// unreported long), an explicit null, a non-integer or a negative value is an error.
func audienceCount(field string, raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	if string(raw) == "null" {
		return 0, fmt.Errorf("%s is an explicit null, not a measured value", field)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("%s is not an integer (%d bytes)", field, len(raw))
	}
	if n < 0 {
		return 0, fmt.Errorf("%s is negative", field)
	}
	return n, nil
}

// audienceCostMicros decodes costInUsd (a BigDecimal rendered as a JSON string) into USD micros
// through costInUsdToMicros: absent is 0, an explicit null or a non-string is an error.
func audienceCostMicros(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	if string(raw) == "null" {
		return 0, errors.New("costInUsd is an explicit null, not a measured value")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("costInUsd is not a string (%d bytes)", len(raw))
	}
	micros, err := costInUsdToMicros(s)
	if err != nil {
		return 0, fmt.Errorf("costInUsd: %w", err)
	}
	return micros, nil
}

// addAudienceCounters sums one element into a bucket, refusing an int64 overflow rather than
// wrapping.
func addAudienceCounters(b *AudienceBucket, impressions, clicks, cost int64) error {
	const limit = int64(^uint64(0) >> 1)
	if b.Impressions > limit-impressions || b.Clicks > limit-clicks || b.CostMicros > limit-cost {
		return errors.New("counters overflow int64 when summed across campaigns")
	}
	b.Impressions += impressions
	b.Clicks += clicks
	b.CostMicros += cost
	return nil
}

// finishAudienceBuckets computes CTR after aggregation and orders by impressions descending, then
// value, so the response is deterministic for a given upstream answer.
func finishAudienceBuckets(totals map[string]*AudienceBucket) []AudienceBucket {
	out := make([]AudienceBucket, 0, len(totals))
	for _, b := range totals {
		if b.Impressions > 0 {
			b.Ctr = float64(b.Clicks) / float64(b.Impressions)
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Impressions != out[j].Impressions {
			return out[i].Impressions > out[j].Impressions
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// getAdAnalyticsRaw performs one authenticated adAnalytics GET and returns the bounded 2xx body.
// It shares the account monitor's single-attempt discipline (fetchAccountCampaignAnalyticsRaw):
// redacted transport errors, the +1 size sentinel, 401 classification through
// expiredCredentialsError, and no upstream body retained on a non-2xx apiError.
func (c *Client) getAdAnalyticsRaw(ctx context.Context, rawURL string) ([]byte, error) {
	attemptCtx, cancel, token, err := c.authorizedAttempt(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("LinkedIn-Version", c.apiVersion)
	req.Header.Set("X-RestLi-Protocol-Version", "2.0.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: "adAnalytics", Err: redactHTTPDoError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &transportError{Method: http.MethodGet, Path: "adAnalytics", Err: redactBodyReadError(err)}
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, &transportError{Method: http.MethodGet, Path: "adAnalytics", Err: fmt.Errorf("response exceeds %d bytes", maxResponseBytes)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ce := c.expiredCredentialsError(resp.StatusCode, string(body), http.MethodGet); ce != nil {
			return nil, ce
		}
		return nil, &apiError{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: "adAnalytics"}
	}
	return body, nil
}
