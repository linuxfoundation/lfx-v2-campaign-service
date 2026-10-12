// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Audience breakdown dimension tokens. These are this broker's vocabulary, not Meta's: each
// names one Insights `breakdowns` combination (audienceBreakdowns below).
const (
	// AudienceDimensionAgeGender is Meta's combined `age,gender` breakdown: every bucket
	// carries both values.
	AudienceDimensionAgeGender = "age_gender"
	// AudienceDimensionPlacement is the `publisher_platform,platform_position` breakdown.
	AudienceDimensionPlacement = "placement"
)

const (
	// audiencePageSize is the Insights `limit` per page.
	audiencePageSize = 500
	// audienceMaxPages bounds one breakdown's walk at audiencePageSize*audienceMaxPages rows.
	// The walk reads at most this many pages; a `paging.next` on the last one is the
	// read-plus-one signal that more exist, and is an ERROR, never a silently truncated
	// answer: a demographic distribution computed over part of the rows looks exactly like a
	// complete one.
	audienceMaxPages = 20
	// MaxAudienceCampaigns bounds the campaign-id list the `filtering` parameter carries.
	// Meta publishes no limit for an IN filter's value list; this is a conservative LOCAL
	// bound on the GET URL the list is encoded into (each id is up to 32 digits plus
	// percent-encoded quoting), not a documented Meta limit.
	MaxAudienceCampaigns = 250
)

// ErrAudienceScopeTooLarge marks a read refused locally because the scope exceeds
// MaxAudienceCampaigns. Permanent while the project owns that many campaigns.
var ErrAudienceScopeTooLarge = errors.New("meta-ads: too many campaigns to scope one audience read to")

// ErrAudienceScopeInvalid marks a read refused locally because a scope entry is not a
// canonical Meta campaign id, or the scope is empty. Refused rather than dropped: a reduced
// scope would be presented as the project's whole picture, and an empty one would widen the
// read to the whole shared ad account.
var ErrAudienceScopeInvalid = errors.New("meta-ads: audience read scope is empty or holds an invalid campaign id")

// audienceValueRE is the charset a breakdown value must match to be returned verbatim. It
// admits every value Meta documents for these breakdowns (age "18-24"/"65+"/"Unknown",
// gender "male"/"female"/"unknown", publisher_platform "audience_network", platform_position
// "instagram_stories", …) and nothing that could carry markup, whitespace or control bytes. A
// value outside it is not dropped or folded: the row is untrustworthy and the read fails.
var audienceValueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_+\-]{0,63}$`)

// currencyRE is an ISO 4217 alphabetic code, the shape Meta's account_currency carries.
var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// audienceBreakdown is one Insights read: the dimension token and the Meta breakdown keys it
// requests, in the order the bucket key is built from.
type audienceBreakdown struct {
	dimension string
	keys      []string
}

// audienceBreakdowns are the two reads, in response order. Both must succeed.
var audienceBreakdowns = []audienceBreakdown{
	{dimension: AudienceDimensionAgeGender, keys: []string{"age", "gender"}},
	{dimension: AudienceDimensionPlacement, keys: []string{"publisher_platform", "platform_position"}},
}

// AudienceBucket is one breakdown segment's counters, summed across the scoped campaigns.
// Only the value fields of its own Dimension are set.
type AudienceBucket struct {
	Dimension         string
	Age               string
	Gender            string
	PublisherPlatform string
	PlatformPosition  string
	Impressions       int64
	Clicks            int64
	// CostMicros is spend in micros of the ad account's currency (AudienceInsights.Currency).
	CostMicros int64
	// Ctr is Clicks/Impressions computed AFTER aggregation, 0 when Impressions is 0.
	Ctr float64
}

// AudienceInsights is the audience read across both breakdowns, confined to the caller's
// campaigns.
type AudienceInsights struct {
	Window MetricsWindow
	// Currency is the ad account's ISO 4217 currency as every row reported it; "" when no
	// row was returned.
	Currency string
	Buckets  []AudienceBucket
}

// audienceRow is one Insights row. The breakdown values are POINTERS so an absent key is
// distinguishable from an empty one; both are refused, but they are different defects.
//
// The three counters are RAW so an ABSENT key is distinguishable from an explicit JSON null.
// Absence keeps the Meta metrics read's semantics — Meta omits a zero-valued counter, so an
// omitted one is a measured 0 (parseMetricInt / parseSpendMicros on "") — but a PRESENT null
// decodes into a plain string as "" too, and would then publish an authoritative zero for a
// value Meta explicitly did not give. counterString refuses it. (campaign_id, account_currency
// and the breakdown values need no such care: a null there is already refused as out of scope,
// not an ISO code, or missing.)
type audienceRow struct {
	CampaignID        string          `json:"campaign_id"`
	Impressions       json.RawMessage `json:"impressions"`
	Clicks            json.RawMessage `json:"clicks"`
	Spend             json.RawMessage `json:"spend"`
	AccountCurrency   string          `json:"account_currency"`
	Age               *string         `json:"age"`
	Gender            *string         `json:"gender"`
	PublisherPlatform *string         `json:"publisher_platform"`
	PlatformPosition  *string         `json:"platform_position"`
}

// counterString returns a counter's string value: "" when the key was absent (a measured zero,
// as in the metrics read), an error for an explicit null or a non-string value.
func counterString(field string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if string(raw) == "null" {
		return "", fmt.Errorf("%s is an explicit null, not a measured value", field)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%s is not a string (%d bytes)", field, len(raw))
	}
	return v, nil
}

func (r *audienceRow) value(key string) *string {
	switch key {
	case "age":
		return r.Age
	case "gender":
		return r.Gender
	case "publisher_platform":
		return r.PublisherPlatform
	case "platform_position":
		return r.PlatformPosition
	}
	return nil
}

// GetAudienceInsights reads the age+gender and placement breakdowns over window, across the
// campaigns named by campaignIDs on accountID — NOT the whole ad account.
//
// Each breakdown is one `GET /act_<id>/insights` at level=campaign, with a `filtering` on
// campaign.id IN campaignIDs. The filter is the tenant boundary on a shared account, and it is
// ALSO checked on the way back: every row must name a campaign in the scope, or the read fails —
// once a foreign row's spend is summed into a bucket it is indistinguishable from the project's
// own. That is why level=campaign is requested at all; an account-level aggregate would carry no
// evidence the filter was honoured.
//
// The window maps to Meta's date_preset through the same allow-list the campaign metrics read
// uses, so Meta resolves it in the ad account's own timezone, exactly as it does there.
//
// All or nothing: a failure on either breakdown, a malformed or duplicated row, an untrustworthy
// breakdown value, or more rows than the page bound fails the whole call. A partial demographic
// picture presented as a whole one is how a campaign gets re-targeted on half the data.
func (c *Client) GetAudienceInsights(ctx context.Context, accountID string, window MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("get audience insights: %w", err)
	}
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	preset, ok := datePresetFor[w]
	if !ok {
		return nil, fmt.Errorf("get audience insights: unsupported window %q", window)
	}
	inScope, filtering, err := audienceScope(campaignIDs)
	if err != nil {
		return nil, err
	}

	out := &AudienceInsights{Window: w, Buckets: []AudienceBucket{}}
	for _, b := range audienceBreakdowns {
		buckets, currency, berr := c.readAudienceBreakdown(ctx, accountID, preset, filtering, inScope, b)
		if berr != nil {
			return nil, fmt.Errorf("get audience insights (%s): %w", b.dimension, berr)
		}
		if currency != "" {
			if out.Currency != "" && out.Currency != currency {
				return nil, fmt.Errorf("get audience insights (%s): rows report different account currencies across breakdowns", b.dimension)
			}
			out.Currency = currency
		}
		out.Buckets = append(out.Buckets, buckets...)
	}
	return out, nil
}

// audienceScope validates the campaign ids and renders the Insights `filtering` value.
// Duplicates collapse; an empty, invalid or oversized scope is refused before any request.
func audienceScope(campaignIDs []string) (map[string]struct{}, string, error) {
	if len(campaignIDs) == 0 {
		return nil, "", fmt.Errorf("get audience insights: %w", ErrAudienceScopeInvalid)
	}
	inScope := make(map[string]struct{}, len(campaignIDs))
	ids := make([]string, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		if err := ValidateCampaignID(id); err != nil {
			return nil, "", fmt.Errorf("get audience insights: %w (%d bytes)", ErrAudienceScopeInvalid, len(id))
		}
		if _, dup := inScope[id]; dup {
			continue
		}
		inScope[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > MaxAudienceCampaigns {
		return nil, "", fmt.Errorf("get audience insights: %d campaigns (at most %d): %w", len(ids), MaxAudienceCampaigns, ErrAudienceScopeTooLarge)
	}
	filter := []map[string]any{{"field": "campaign.id", "operator": "IN", "value": ids}}
	raw, err := json.Marshal(filter)
	if err != nil {
		return nil, "", fmt.Errorf("get audience insights: encode filtering: %w", err)
	}
	return inScope, string(raw), nil
}

// readAudienceBreakdown walks one breakdown's pages and aggregates its rows by segment.
func (c *Client) readAudienceBreakdown(ctx context.Context, accountID, preset, filtering string, inScope map[string]struct{}, b audienceBreakdown) ([]AudienceBucket, string, error) {
	totals := map[string]*AudienceBucket{}
	order := []string{}
	seenRows := map[string]struct{}{}
	seenCursors := map[string]struct{}{}
	currency := ""
	after := ""
	for page := 0; page < audienceMaxPages; page++ {
		// Every interpolated value is a constant, the allow-listed preset, the validated
		// account id, or a query-escaped value — no caller input reaches the path unescaped.
		path := "/" + accountID + "/insights?level=campaign" +
			"&fields=campaign_id,impressions,clicks,spend,account_currency" +
			"&breakdowns=" + strings.Join(b.keys, ",") +
			"&date_preset=" + preset +
			"&filtering=" + url.QueryEscape(filtering) +
			"&limit=" + strconv.Itoa(audiencePageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			// Pointer so an absent `data` (a malformed 2xx proving nothing) is distinct from
			// Meta's authoritative empty `{"data":[]}`; see insightsResponse.
			Data   *[]json.RawMessage `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		// The page is fetched RAW and checked as a whole before it is decoded. encoding/json
		// resolves a repeated key — exact or case-folded, at ANY level — in favour of the last
		// value with no error, so `{"data":[…rows…],"data":[]}` (or `"Data":[]`) would decode as
		// Meta's authoritative empty answer and a populated audience would read as no delivery;
		// a repeated `paging` could likewise end the walk early. identityjson.Check refuses
		// that (and malformed UTF-8 / unpaired surrogates) with one linear, map-based pass over
		// the document, under the decoder's own notion of key sameness.
		var body json.RawMessage
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &body); err != nil {
			return nil, "", err
		}
		malformed := func(format string, args ...any) error {
			return &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf(format, args...)}
		}
		if err := identityjson.Check(body); err != nil {
			return nil, "", malformed("page %d: %v", page, err)
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			// The decoder's message can quote a value; report the shape only.
			return nil, "", malformed("page %d: response is not an insights page of the expected shape", page)
		}
		if resp.Data == nil {
			return nil, "", malformed("insights returned a 2xx response with no data field")
		}
		for i, raw := range *resp.Data {
			var row audienceRow
			if err := json.Unmarshal(raw, &row); err != nil {
				// The decoder's message can quote a value; report the shape only.
				return nil, "", malformed("page %d row %d: row is not an insights object of the expected shape", page, i)
			}
			if _, ok := inScope[row.CampaignID]; !ok {
				// Not echoed: the id is unvalidated upstream content.
				return nil, "", malformed("page %d row %d: row names a campaign outside the requested scope (%d bytes)", page, i, len(row.CampaignID))
			}
			if !currencyRE.MatchString(row.AccountCurrency) {
				return nil, "", malformed("page %d row %d: account_currency is not an ISO 4217 code (%d bytes)", page, i, len(row.AccountCurrency))
			}
			if currency != "" && currency != row.AccountCurrency {
				return nil, "", malformed("page %d row %d: rows report different account currencies", page, i)
			}
			currency = row.AccountCurrency
			values := make([]string, len(b.keys))
			for k, key := range b.keys {
				v := row.value(key)
				if v == nil {
					return nil, "", malformed("page %d row %d: breakdown %q is missing", page, i, key)
				}
				if !audienceValueRE.MatchString(*v) {
					return nil, "", malformed("page %d row %d: breakdown %q value is outside the accepted charset (%d bytes)", page, i, key, len(*v))
				}
				values[k] = *v
			}
			segment := strings.Join(values, "\x00")
			rowKey := row.CampaignID + "\x00" + segment
			if _, dup := seenRows[rowKey]; dup {
				// level=campaign yields one row per (campaign, segment). A repeat means
				// overlapping pages or a double-reported row; summing it would double-count.
				return nil, "", malformed("page %d row %d: duplicate row for one campaign and segment", page, i)
			}
			seenRows[rowKey] = struct{}{}
			impressionsS, errIS := counterString("impressions", row.Impressions)
			clicksS, errCS := counterString("clicks", row.Clicks)
			spendS, errSS := counterString("spend", row.Spend)
			if err := errors.Join(errIS, errCS, errSS); err != nil {
				return nil, "", malformed("page %d row %d: %v", page, i, err)
			}
			impressions, errI := parseMetricInt(impressionsS)
			clicks, errC := parseMetricInt(clicksS)
			if errI != nil || errC != nil {
				return nil, "", malformed("page %d row %d: %s", page, i, malformedCounterSummary(errI, errC))
			}
			cost, errS := parseSpendMicros(spendS)
			if errS != nil {
				return nil, "", malformed("page %d row %d: %v", page, i, errS)
			}
			bucket, ok := totals[segment]
			if !ok {
				bucket = newAudienceBucket(b.dimension, values)
				totals[segment] = bucket
				order = append(order, segment)
			}
			if err := addCounters(bucket, impressions, clicks, cost); err != nil {
				return nil, "", malformed("page %d row %d: %v", page, i, err)
			}
		}
		if resp.Paging.Next == "" {
			return finishAudienceBuckets(totals, order), currency, nil
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, "", fmt.Errorf("insights has more pages but no cursor; cannot guarantee every row was read")
		}
		if _, dup := seenCursors[after]; dup {
			return nil, "", fmt.Errorf("insights paging did not terminate (repeated cursor)")
		}
		seenCursors[after] = struct{}{}
	}
	return nil, "", fmt.Errorf("insights exceeded %d pages of %d rows; refusing a partial audience read", audienceMaxPages, audiencePageSize)
}

func newAudienceBucket(dimension string, values []string) *AudienceBucket {
	bucket := &AudienceBucket{Dimension: dimension}
	switch dimension {
	case AudienceDimensionAgeGender:
		bucket.Age, bucket.Gender = values[0], values[1]
	case AudienceDimensionPlacement:
		bucket.PublisherPlatform, bucket.PlatformPosition = values[0], values[1]
	}
	return bucket
}

// addCounters sums one row into a bucket, refusing an int64 overflow rather than wrapping.
func addCounters(b *AudienceBucket, impressions, clicks, cost int64) error {
	const limit = int64(^uint64(0) >> 1)
	if b.Impressions > limit-impressions || b.Clicks > limit-clicks || b.CostMicros > limit-cost {
		return fmt.Errorf("counters overflow int64 when summed across campaigns")
	}
	b.Impressions += impressions
	b.Clicks += clicks
	b.CostMicros += cost
	return nil
}

// finishAudienceBuckets computes CTR after aggregation (summing per-row CTRs would weight a
// ten-impression campaign like a thousand-impression one) and orders by impressions
// descending, then segment, so the response is deterministic for a given upstream answer.
func finishAudienceBuckets(totals map[string]*AudienceBucket, order []string) []AudienceBucket {
	out := make([]AudienceBucket, 0, len(order))
	for _, k := range order {
		b := totals[k]
		if b.Impressions > 0 {
			b.Ctr = float64(b.Clicks) / float64(b.Impressions)
		}
		out = append(out, *b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Impressions != out[j].Impressions {
			return out[i].Impressions > out[j].Impressions
		}
		return audienceSortKey(out[i]) < audienceSortKey(out[j])
	})
	return out
}

func audienceSortKey(b AudienceBucket) string {
	return b.Age + "\x00" + b.Gender + "\x00" + b.PublisherPlatform + "\x00" + b.PlatformPosition
}
