// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

const (
	// MaxKeywordRows caps the keyword read's rows, ranked by impressions, as the Google and
	// Microsoft keyword reads cap theirs; KeywordPerformance.Truncated says when more exist.
	MaxKeywordRows = 50
	// maxKeywordTextLen bounds a reported keyword, matching the 200 the keyword-targeting removal
	// (remove-keyword-targeting) accepts for the same text.
	maxKeywordTextLen = 200
)

// keywordBreakdown is the KEYWORD report breakdown. Per the spec it is in both the `breakdowns` and
// `fields` enums, and the Metric schema's `keyword` is "The keyword for the ad." — a
// ["string","null"] TEXT value (example "internet"). Nothing on the row identifies a targeting
// entry: no keyword id exists in the Metric schema, so a row is read-only.
var keywordBreakdown = reportBreakdown{dimension: "keyword", breakdown: "KEYWORD", rowKey: "keyword", valueErr: keywordTextErr}

// keywordTextErr admits keyword text verbatim — spaces, punctuation and non-ASCII letters included,
// since targeted keywords are phrases ("service mesh") — while refusing what could not be a keyword
// and could carry markup or break a log line: invalid UTF-8, a control character, `<` or `>`, a
// blank value, or one longer than maxKeywordTextLen bytes. A refused value fails the whole read.
func keywordTextErr(v string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("keyword is blank (%d bytes)", len(v))
	case len(v) > maxKeywordTextLen:
		return fmt.Errorf("keyword is longer than %d bytes (%d bytes)", maxKeywordTextLen, len(v))
	case !utf8.ValidString(v):
		return fmt.Errorf("keyword is not valid UTF-8 (%d bytes)", len(v))
	case strings.ContainsAny(v, "<>"):
		return fmt.Errorf("keyword contains markup characters (%d bytes)", len(v))
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("keyword contains a control character (%d bytes)", len(v))
		}
	}
	return nil
}

// KeywordRow is one (campaign, keyword) pair's counters over the window.
type KeywordRow struct {
	CampaignID string
	// Text is Reddit's KEYWORD value verbatim; "" when Reddit reported an explicit null (delivery
	// it attributed to no keyword — e.g. served on community or interest targeting).
	Text        string
	Impressions int64
	Clicks      int64
	// CostMicros is SPEND, already microcurrency, in the ad account's own currency.
	CostMicros int64
	// Ctr is Clicks/Impressions, 0 when Impressions is 0.
	Ctr float64
}

// KeywordPerformance is the keyword read across the caller's campaigns.
type KeywordPerformance struct {
	Window model.MetricsWindow
	// Rows are the top MaxKeywordRows (campaign, keyword) pairs by impressions.
	Rows []KeywordRow
	// Truncated reports that more rows existed than were returned.
	Truncated bool
}

// GetKeywordPerformance reads the KEYWORD report breakdown over window for each campaign named by
// campaignIDs on this client's bound ad account — NOT the whole account — through the shared
// broken-down read (report_breakdown.go): one filtered report per campaign, raw-body identityjson
// check, fail closed on any untrustworthy row, at most MaxReportCampaigns campaigns.
//
// Rows stay per (campaign, keyword) rather than summed across campaigns: the only action Reddit
// offers on a keyword is removing it from ONE campaign's ad-group targeting, so a row must say
// which campaign it served in. They are ordered by impressions descending, then campaign id, then
// keyword (the unattributed row last among equals), and capped at MaxKeywordRows.
func (c *Client) GetKeywordPerformance(ctx context.Context, window model.MetricsWindow, campaignIDs []string) (*KeywordPerformance, error) {
	r, err := c.prepareBreakdownRead("get keyword performance", window, campaignIDs)
	if err != nil {
		return nil, err
	}
	results, err := c.readBreakdownReports(ctx, r, []reportBreakdown{keywordBreakdown})
	if err != nil {
		return nil, err
	}
	rows := []KeywordRow{}
	for i, perKeyword := range results[0] {
		for text, t := range perKeyword {
			row := KeywordRow{CampaignID: r.campaignIDs[i], Text: text, Impressions: t.Impressions, Clicks: t.Clicks, CostMicros: t.CostMicros}
			if row.Impressions > 0 {
				row.Ctr = float64(row.Clicks) / float64(row.Impressions)
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		if a.CampaignID != b.CampaignID {
			return a.CampaignID < b.CampaignID
		}
		if (a.Text == "") != (b.Text == "") {
			return b.Text == ""
		}
		return a.Text < b.Text
	})
	out := &KeywordPerformance{Window: window, Rows: rows}
	if len(rows) > MaxKeywordRows {
		out.Rows = rows[:MaxKeywordRows]
		out.Truncated = true
	}
	return out, nil
}
