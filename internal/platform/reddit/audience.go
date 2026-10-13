// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Audience dimension tokens. These are this broker's vocabulary; each names one Reddit Ads API v3
// report `breakdowns` member (audienceBreakdowns below).
const (
	// AudienceDimensionGender is the GENDER breakdown.
	AudienceDimensionGender = "gender"
	// AudienceDimensionCountry is the COUNTRY breakdown.
	AudienceDimensionCountry = "country"
	// AudienceDimensionCommunity is the COMMUNITY breakdown (the subreddit).
	AudienceDimensionCommunity = "community"
)

const (
	// MaxAudienceCampaigns bounds the campaigns one audience read covers. A LOCAL bound, not a
	// Reddit limit: the read issues one report per campaign per breakdown (see
	// GetAudienceInsights for why), and Reddit's published rate limit for the reporting policy
	// (`ads-reporting`) is 60 requests per 60 seconds per caller. 8 campaigns × 3 breakdowns = 24
	// requests, under half the quota, leaving the rest for the campaign metrics read and the
	// account monitor, which share it.
	MaxAudienceCampaigns = 8
	// audienceReportConcurrency bounds the reports in flight at once: enough to fit 24 requests
	// in the orchestrator's 20s metrics budget, below the monitor's own 5.
	audienceReportConcurrency = 3
	// audienceMaxPages bounds one report's pagination walk. One campaign broken down by one
	// dimension is at most a few hundred rows (Reddit's countries or the communities it served
	// in), so a walk that is still paging after this many pages is not a result this read can
	// trust to be complete — and a partial distribution looks exactly like a whole one.
	audienceMaxPages = 10
)

// ErrAudienceScopeTooLarge marks a read refused locally because the scope exceeds
// MaxAudienceCampaigns. Permanent while the project owns that many campaigns.
var ErrAudienceScopeTooLarge = errors.New("reddit-ads: too many campaigns to scope one audience read to")

// ErrAudienceScopeInvalid marks a read refused locally because a scope entry is not a valid
// Reddit campaign id, or the scope is empty. Refused rather than dropped: a reduced scope would be
// presented as the project's whole picture, and an empty one has no campaign to filter on.
var ErrAudienceScopeInvalid = errors.New("reddit-ads: audience read scope is empty or holds an invalid campaign id")

// errAudiencePageCap is the walk's refusal once audienceMaxPages pages still point at another.
var errAudiencePageCap = fmt.Errorf("report still had more pages after %d; refusing a partial audience read", audienceMaxPages)

// audienceValueRE is the charset a breakdown value must match to be returned verbatim. It admits
// the values Reddit's spec documents for these breakdowns (gender "female", country "US",
// community "AskReddit") and subreddit names generally (letters, digits, underscores), and nothing
// that could carry markup, whitespace or control bytes. A value outside it is not dropped or
// folded: the row is untrustworthy and the read fails.
var audienceValueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,63}$`)

// audienceBreakdown is one report shape: the dimension token, the spec's `breakdowns` enum member,
// and the row key the breakdown value arrives under.
type audienceBreakdown struct {
	dimension string
	breakdown string
	rowKey    string
}

// audienceBreakdowns are the three reads, in response order. All must succeed.
//
// Every member is in BOTH of the spec's GetReportRequestBody enums — `breakdowns` (to group by it)
// and `fields` (so the value is echoed onto each row, as CAMPAIGN_ID is for provenance) — and its
// value is a documented property of the response's Metric schema. AGE is deliberately absent: the
// `fields` enum and the Metric schema carry it, but the `breakdowns` enum does not, so a report
// cannot be grouped by it.
var audienceBreakdowns = []audienceBreakdown{
	{dimension: AudienceDimensionGender, breakdown: "GENDER", rowKey: "gender"},
	{dimension: AudienceDimensionCountry, breakdown: "COUNTRY", rowKey: "country"},
	{dimension: AudienceDimensionCommunity, breakdown: "COMMUNITY", rowKey: "community"},
}

// AudienceBucket is one breakdown value's counters, summed across the scoped campaigns.
type AudienceBucket struct {
	Dimension string
	// Value is Reddit's breakdown value verbatim; "" when Reddit reported an explicit null for the
	// dimension (traffic it did not attribute to any value). Never "" otherwise: a present value
	// must match audienceValueRE, which refuses the empty string.
	Value       string
	Impressions int64
	Clicks      int64
	// CostMicros is SPEND, already microcurrency per the spec, in the ad account's own currency.
	CostMicros int64
	// Ctr is Clicks/Impressions computed AFTER aggregation, 0 when Impressions is 0.
	Ctr float64
}

// AudienceInsights is the audience read across all three breakdowns, confined to the caller's
// campaigns.
type AudienceInsights struct {
	Window  model.MetricsWindow
	Buckets []AudienceBucket
}

// audienceReportRow is one entry of a broken-down report's `metrics` array: the four fields every
// report row here carries (reportRow, validated by the same reportRow.validate the campaign
// metrics read uses — a missing OR explicit-null counter is refused, never read as 0) plus the
// three breakdown values, kept RAW so an absent key (the breakdown was not honoured — refused) is
// distinguishable from an explicit JSON null (the spec types each as ["string","null"] — a real,
// unattributed bucket).
type audienceReportRow struct {
	reportRow
	Gender    json.RawMessage `json:"gender"`
	Country   json.RawMessage `json:"country"`
	Community json.RawMessage `json:"community"`
}

func (r *audienceReportRow) value(rowKey string) json.RawMessage {
	switch rowKey {
	case "gender":
		return r.Gender
	case "country":
		return r.Country
	case "community":
		return r.Community
	}
	return nil
}

// GetAudienceInsights reads the GENDER, COUNTRY and COMMUNITY report breakdowns over window, across
// the campaigns named by campaignIDs on this client's bound ad account — NOT the whole account.
//
// Each (campaign, breakdown) pair is ONE `POST /ad_accounts/{id}/reports` with
// `filter: campaign:id==<id>`, `breakdowns: [<DIM>]` and `fields: [CAMPAIGN_ID, IMPRESSIONS,
// CLICKS, SPEND, <DIM>]`. One campaign per request, rather than one request listing every
// campaign, because the single-campaign filter term is the only filter form this repository has
// verified (the campaign metrics read and the account monitor use it): the spec says only that
// `filter` is "a comma-separated list", without saying whether two terms on the same field are
// OR-ed or AND-ed, and an AND would silently answer an empty report — no delivery — for a project
// that had plenty. The filter is ALSO checked on the way back: every row must name the requested
// campaign (reportRow.validate), or the read fails.
//
// The window maps to starts_at/ends_at through dateRangeForWindow, the campaign metrics read's
// mapping, so both reads cover the same UTC days for the same window name.
//
// All or nothing: a failure on any report, a response with a duplicated key (identityjson.Check on
// the raw body, before decoding), a malformed, duplicated or foreign row, a missing or null
// counter, an untrustworthy breakdown value, or more pages than audienceMaxPages fails the whole
// call. A partial audience presented as a whole one is how a campaign gets re-targeted on half
// the data.
func (c *Client) GetAudienceInsights(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	startsAt, endsAt, err := dateRangeForWindow(window, c.now())
	if err != nil {
		return nil, fmt.Errorf("get audience insights: %w", err)
	}
	accountID := strings.TrimSpace(c.account.AccountID)
	if err := ValidateAccountID(accountID); err != nil {
		// Configuration, not upstream content — but not echoed: this error reaches a log line.
		return nil, fmt.Errorf("get audience insights: %w", err)
	}
	ids, err := audienceScope(campaignIDs)
	if err != nil {
		return nil, err
	}

	// results[b][i] is breakdown b's per-value totals for campaign ids[i]; each goroutine writes
	// only its own cell.
	results := make([][]map[string]*AudienceBucket, len(audienceBreakdowns))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(audienceReportConcurrency)
	for bi, b := range audienceBreakdowns {
		results[bi] = make([]map[string]*AudienceBucket, len(ids))
		for i, id := range ids {
			g.Go(func() error {
				totals, rerr := c.readAudienceReport(gctx, accountID, startsAt, endsAt, id, b)
				if rerr != nil {
					return fmt.Errorf("get audience insights (%s): %w", b.dimension, redactReportPath(rerr, accountID))
				}
				results[bi][i] = totals
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := &AudienceInsights{Window: window, Buckets: []AudienceBucket{}}
	for bi, b := range audienceBreakdowns {
		merged := map[string]*AudienceBucket{}
		for _, perCampaign := range results[bi] {
			for value, part := range perCampaign {
				bucket, ok := merged[value]
				if !ok {
					bucket = &AudienceBucket{Dimension: b.dimension, Value: value}
					merged[value] = bucket
				}
				if err := addAudienceCounters(bucket, part.Impressions, part.Clicks, part.CostMicros); err != nil {
					return nil, fmt.Errorf("get audience insights (%s): %w", b.dimension, err)
				}
			}
		}
		out.Buckets = append(out.Buckets, finishAudienceBuckets(merged)...)
	}
	return out, nil
}

// audienceScope validates the campaign ids, collapsing duplicates. An empty, invalid or oversized
// scope is refused before any request.
func audienceScope(campaignIDs []string) ([]string, error) {
	if len(campaignIDs) == 0 {
		return nil, fmt.Errorf("get audience insights: %w", ErrAudienceScopeInvalid)
	}
	seen := make(map[string]struct{}, len(campaignIDs))
	ids := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		// ValidateCampaignID also keeps the id out of the `filter` DSL's syntax: a comma would
		// split one term into two and widen the report.
		if err := ValidateCampaignID(id); err != nil {
			return nil, fmt.Errorf("get audience insights: %w (%d bytes)", ErrAudienceScopeInvalid, len(id))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > MaxAudienceCampaigns {
		return nil, fmt.Errorf("get audience insights: %d campaigns (at most %d): %w", len(ids), MaxAudienceCampaigns, ErrAudienceScopeTooLarge)
	}
	return ids, nil
}

// readAudienceReport walks one (campaign, breakdown) report's pages and totals its rows by
// breakdown value. An explicit empty `metrics` array is real zero delivery (no buckets); a missing
// or null one is an error (decodeReportRows' rule).
func (c *Client) readAudienceReport(ctx context.Context, accountID, startsAt, endsAt, campaignID string, b audienceBreakdown) (map[string]*AudienceBucket, error) {
	reqBody := map[string]any{
		"data": map[string]any{
			"starts_at":  startsAt,
			"ends_at":    endsAt,
			"fields":     []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND", b.breakdown},
			"breakdowns": []string{b.breakdown},
			"filter":     "campaign:id==" + campaignID,
		},
	}
	totals := map[string]*AudienceBucket{}
	werr := c.walkPagesCapped(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/reports", reqBody, audienceMaxPages, errAudiencePageCap, func(page int, resp *apiResponse) error {
		// The page is checked RAW, as a whole, before anything is decoded. encoding/json resolves a
		// repeated key — exact or case-folded, at ANY level — in favour of the last value with no
		// error, so `{"data":{"metrics":[…rows…]},"data":{"metrics":[]}}` would decode as Reddit's
		// authoritative empty report and a populated audience would read as no delivery; a repeated
		// `impressions` inside a row would replace one counter with another. resp.raw is the body
		// the envelope (Data, Pagination) was decoded from.
		if err := identityjson.Check(resp.raw); err != nil {
			return audienceDecodeError(fmt.Errorf("page %d: %w", page, err))
		}
		raws, derr := decodeAudienceRows(resp.Data)
		if derr != nil {
			return audienceDecodeError(fmt.Errorf("page %d: %w", page, derr))
		}
		for i, raw := range raws {
			var row audienceReportRow
			if err := json.Unmarshal(raw, &row); err != nil {
				// The decoder's message can quote a value; report the shape only.
				return audienceDecodeError(fmt.Errorf("page %d row %d: row is not a report metric object of the expected shape (%d bytes)", page, i, len(raw)))
			}
			if err := row.validate(campaignID); err != nil {
				return audienceDecodeError(fmt.Errorf("page %d row %d: %w", page, i, err))
			}
			value, verr := audienceValue(row.value(b.rowKey), b.rowKey)
			if verr != nil {
				return audienceDecodeError(fmt.Errorf("page %d row %d: %w", page, i, verr))
			}
			if _, dup := totals[value]; dup {
				// One campaign broken down by one dimension yields one row per value. A repeat
				// means overlapping pages or a double-reported row; summing it would double-count.
				return audienceDecodeError(fmt.Errorf("page %d row %d: duplicate row for one breakdown value", page, i))
			}
			totals[value] = &AudienceBucket{Dimension: b.dimension, Value: value, Impressions: *row.Impressions, Clicks: *row.Clicks, CostMicros: *row.Spend}
		}
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return totals, nil
}

// decodeAudienceRows is decodeReportRows for raw rows: a missing, null or non-object `data`, or a
// missing or null `metrics` array, is an error; only an explicit array (possibly empty) is a report.
func decodeAudienceRows(data json.RawMessage) ([]json.RawMessage, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, errors.New("response has no \"data\" object")
	}
	var env struct {
		Metrics *[]json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("\"data\" is not a report object (%d bytes)", len(data))
	}
	if env.Metrics == nil {
		return nil, errors.New("\"data\" has no \"metrics\" array")
	}
	return *env.Metrics, nil
}

// audienceValue reads one breakdown value: an absent key is an error (the report was not grouped
// by the requested breakdown, so its rows cannot be attributed to a value), an explicit null is the
// unattributed bucket "", and a string must match audienceValueRE. No value is echoed in an error.
func audienceValue(raw json.RawMessage, key string) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("breakdown %q is missing from the row", key)
	}
	if string(raw) == "null" {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("breakdown %q is not a string (%d bytes)", key, len(raw))
	}
	if !audienceValueRE.MatchString(v) {
		return "", fmt.Errorf("breakdown %q value is outside the accepted charset (%d bytes)", key, len(v))
	}
	return v, nil
}

// audienceDecodeError is reportDecodeError for the audience read: a 2xx whose body could not be
// believed, classified as a transport failure (→ 503), with the literal path "reports" so no
// account id reaches a log line.
func audienceDecodeError(err error) error {
	return &transportError{
		Method: http.MethodPost,
		Path:   "reports",
		Err:    fmt.Errorf("decode audience report response: %w", err),
	}
}

// addAudienceCounters sums one campaign's part into a bucket, refusing an int64 overflow rather
// than wrapping.
func addAudienceCounters(b *AudienceBucket, impressions, clicks, cost int64) error {
	if b.Impressions > math.MaxInt64-impressions || b.Clicks > math.MaxInt64-clicks || b.CostMicros > math.MaxInt64-cost {
		return errors.New("counters overflow int64 when summed across campaigns")
	}
	b.Impressions += impressions
	b.Clicks += clicks
	b.CostMicros += cost
	return nil
}

// finishAudienceBuckets computes CTR after aggregation (averaging per-campaign CTRs would weight a
// ten-impression campaign like a thousand-impression one) and orders by impressions descending,
// then value, the unattributed bucket last among equals — deterministic for a given upstream
// answer.
func finishAudienceBuckets(totals map[string]*AudienceBucket) []AudienceBucket {
	out := make([]AudienceBucket, 0, len(totals))
	for _, b := range totals {
		if b.Impressions > 0 {
			b.Ctr = float64(b.Clicks) / float64(b.Impressions)
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		if (a.Value == "") != (b.Value == "") {
			return b.Value == ""
		}
		return a.Value < b.Value
	})
	return out
}
