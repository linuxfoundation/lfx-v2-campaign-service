// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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

// audienceValueRE is the charset an audience breakdown value must match to be returned verbatim.
// It admits the values Reddit's spec documents for these breakdowns (gender "female", country
// "US", community "AskReddit") and subreddit names generally (letters, digits, underscores), and
// nothing that could carry markup, whitespace or control bytes. A value outside it is not dropped
// or folded: the row is untrustworthy and the read fails.
var audienceValueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,63}$`)

func audienceValueErr(v string) error {
	if !audienceValueRE.MatchString(v) {
		return fmt.Errorf("value is outside the accepted charset (%d bytes)", len(v))
	}
	return nil
}

// audienceBreakdowns are the three reads, in response order. All must succeed.
//
// Every member is in BOTH of the spec's GetReportRequestBody enums — `breakdowns` (to group by it)
// and `fields` (so the value is echoed onto each row) — and its value is a documented property of
// the response's Metric schema. AGE is deliberately absent: the `fields` enum and the Metric schema
// carry it, but the `breakdowns` enum does not, so a report cannot be grouped by it.
var audienceBreakdowns = []reportBreakdown{
	{dimension: AudienceDimensionGender, breakdown: "GENDER", rowKey: "gender", valueErr: audienceValueErr},
	{dimension: AudienceDimensionCountry, breakdown: "COUNTRY", rowKey: "country", valueErr: audienceValueErr},
	{dimension: AudienceDimensionCommunity, breakdown: "COMMUNITY", rowKey: "community", valueErr: audienceValueErr},
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

// GetAudienceInsights reads the GENDER, COUNTRY and COMMUNITY report breakdowns over window, across
// the campaigns named by campaignIDs on this client's bound ad account — NOT the whole account.
//
// Every request, refusal and guard is the shared broken-down read's (report_breakdown.go: one
// filtered report per campaign per breakdown, raw-body identityjson check, fail closed on any
// untrustworthy row, at most MaxReportCampaigns campaigns). This read then sums each breakdown
// value across campaigns. All or nothing: a partial audience presented as a whole one is how a
// campaign gets re-targeted on half the data.
func (c *Client) GetAudienceInsights(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	r, err := c.prepareBreakdownRead("get audience insights", window, campaignIDs)
	if err != nil {
		return nil, err
	}
	results, err := c.readBreakdownReports(ctx, r, audienceBreakdowns)
	if err != nil {
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
				if err := addAudienceCounters(bucket, part); err != nil {
					return nil, fmt.Errorf("get audience insights (%s): %w", b.dimension, err)
				}
			}
		}
		out.Buckets = append(out.Buckets, finishAudienceBuckets(merged)...)
	}
	return out, nil
}

// addAudienceCounters sums one campaign's part into a bucket, refusing an int64 overflow rather
// than wrapping.
func addAudienceCounters(b *AudienceBucket, part breakdownTotals) error {
	if b.Impressions > math.MaxInt64-part.Impressions || b.Clicks > math.MaxInt64-part.Clicks || b.CostMicros > math.MaxInt64-part.CostMicros {
		return errors.New("counters overflow int64 when summed across campaigns")
	}
	b.Impressions += part.Impressions
	b.Clicks += part.Clicks
	b.CostMicros += part.CostMicros
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
