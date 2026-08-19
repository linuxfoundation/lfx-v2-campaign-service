// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Keyword + audience INSIGHTS (project-scoped reads) and keyword ACTIONS
// (pause/remove on an existing ad-group criterion).
//
// Everything here is scoped to the client's own customer id. These are reads of,
// and mutations to, resources that already exist upstream — nothing in this file
// creates a keyword. Criterion creation is GA-4's createAdGroupTargeting.
// ---------------------------------------------------------------------------

const (
	// maxKeywordRows caps the keyword performance read. The GAQL query orders by
	// impressions descending and asks for one row beyond the cap, so a full page is
	// distinguishable from a truncated one — see GetKeywordPerformance.
	//
	// A cap is necessary because an account's keyword set is unbounded and this is a
	// synchronous read feeding a UI table. It is documented in the API contract via the
	// `truncated` flag rather than hidden: a silently-capped total presented as the
	// project's whole spend is a wrong number, not merely an incomplete one.
	maxKeywordRows = 50

	// maxKeywordActions bounds one keyword-actions batch. It matches maxKeywords (the
	// create path's cap) deliberately: the two surfaces address the same criteria, and a
	// caller able to create 60 keywords in one dispatch must be able to pause the same 60
	// in one request. The design layer declares the identical bound so an over-long batch
	// is refused by the generated decoder before any handler runs.
	maxKeywordActions = 60

	// KeywordStatusUnknown and MatchTypeUnknown are the contract's escape hatches for an
	// upstream value this client does not recognise. They are declared here rather than
	// alongside StatusEnabled/MatchTypeExact because they are NOT Google vocabulary — nothing
	// is ever SENT with these values; they exist only so a read can report an unrecognised
	// upstream value without emitting one the API contract forbids.
	KeywordStatusUnknown = "UNKNOWN"
	MatchTypeUnknown     = "UNKNOWN"

	// KeywordActionPause and KeywordActionRemove are the two supported keyword mutations.
	//
	// There is deliberately no "enable": re-enabling a keyword makes a campaign spend
	// again, and this client's keyword-action surface exists to reduce what serves, never
	// to widen it. Widening goes through the create/dispatch path where budget and flight
	// are validated together.
	KeywordActionPause = "PAUSE"
	// KeywordActionRemove is IRREVERSIBLE upstream: Google cannot re-enable a removed
	// ad-group criterion, only create a new one with a new id.
	KeywordActionRemove = "REMOVE"
)

// KeywordRow is one keyword's performance over a window, as read from Google Ads.
type KeywordRow struct {
	CriterionID string `json:"criterionId"`
	AdGroupID   string `json:"adGroupId"`
	CampaignID  string `json:"campaignId"`
	Text        string `json:"text"`
	MatchType   string `json:"matchType"`
	Status      string `json:"status"`
	Impressions int64  `json:"impressions"`
	Clicks      int64  `json:"clicks"`
	CostMicros  int64  `json:"costMicros"`
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64 `json:"ctr"`
}

// KeywordPerformance is the keyword read, confined to the caller's own campaigns.
type KeywordPerformance struct {
	Window MetricsWindow `json:"window"`
	Rows   []KeywordRow  `json:"rows"`
	// Truncated reports that the account holds MORE keywords than Rows carries. Without
	// it a caller receiving exactly maxKeywordRows rows cannot tell a small account from a
	// truncated large one, and would total the slice as if it were the whole account.
	Truncated bool `json:"truncated"`
}

// AudienceBucket is one demographic slice's counters.
type AudienceBucket struct {
	// Dimension is "age", "gender" or "device" — which breakdown Value belongs to.
	Dimension   string  `json:"dimension"`
	Value       string  `json:"value"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	CostMicros  int64   `json:"costMicros"`
	Ctr         float64 `json:"ctr"`
}

// AudienceInsights is the demographic read across all three breakdowns, confined to the
// caller's own campaigns.
type AudienceInsights struct {
	Window  MetricsWindow    `json:"window"`
	Buckets []AudienceBucket `json:"buckets"`
}

// Audience breakdown dimension tokens. These are this broker's vocabulary, not Google's
// — each maps to a different GAQL resource below.
const (
	DimensionAge    = "age"
	DimensionGender = "gender"
	DimensionDevice = "device"
)

// gaqlKeywordRow is one googleAds:search row for the keyword-performance query.
// Every numeric field is a string: Google Ads REST emits int64 as JSON strings to
// avoid float64 precision loss (same as gaqlMetricsRow).
type gaqlKeywordRow struct {
	AdGroupCriterion struct {
		CriterionID string `json:"criterionId"`
		Status      string `json:"status"`
		Keyword     struct {
			Text      string `json:"text"`
			MatchType string `json:"matchType"`
		} `json:"keyword"`
	} `json:"adGroupCriterion"`
	AdGroup struct {
		ID string `json:"id"`
	} `json:"adGroup"`
	Campaign struct {
		ID string `json:"id"`
	} `json:"campaign"`
	Metrics gaqlMetricRowMetrics `json:"metrics"`
}

// gaqlMetricRowMetrics is the metrics block shared by the keyword and audience queries.
type gaqlMetricRowMetrics struct {
	Impressions string `json:"impressions"`
	Clicks      string `json:"clicks"`
	CostMicros  string `json:"costMicros"`
}

// parseRowMetrics parses the three metric strings, naming only the FIELDS that failed
// and never their values — the values come straight from the upstream response body and
// the service renders this error into a log line, so echoing them would let a malformed
// metric inject attacker-influenced text (including newlines) into the log stream. Same
// reasoning as GetCampaignMetrics.
func parseRowMetrics(m gaqlMetricRowMetrics, describe string) (impressions, clicks, costMicros int64, err error) {
	impressions, errImpressions := parseMetricInt(m.Impressions)
	clicks, errClicks := parseMetricInt(m.Clicks)
	costMicros, errCost := parseMetricInt(m.CostMicros)
	if errImpressions != nil || errClicks != nil || errCost != nil {
		var bad []string
		if errImpressions != nil {
			bad = append(bad, "impressions")
		}
		if errClicks != nil {
			bad = append(bad, "clicks")
		}
		if errCost != nil {
			bad = append(bad, "costMicros")
		}
		return 0, 0, 0, fmt.Errorf("%s: unparseable metric field(s): %s", describe, strings.Join(bad, ", "))
	}
	return impressions, clicks, costMicros, nil
}

// ctrFor is Clicks/Impressions, 0 when Impressions is 0, so no caller divides by zero.
// Shared by the keyword and audience row builders in this file. GetCampaignMetrics still
// computes the same expression inline (metrics.go) — deliberately left alone here rather
// than refactored in a change that does not otherwise touch that path.
func ctrFor(impressions, clicks int64) float64 {
	if impressions <= 0 {
		return 0
	}
	return float64(clicks) / float64(impressions)
}

// resolveWindow applies the default and the injection allow-list. The window literal is
// concatenated into the GAQL string (GAQL has no query parameters), so an unvalidated
// caller-supplied value is a query-injection vector exactly as an unvalidated id is.
func resolveWindow(window MetricsWindow, describe string) (MetricsWindow, error) {
	w := window
	if w == "" {
		w = defaultMetricsWindow
	}
	if _, ok := validMetricsWindows[w]; !ok {
		return "", fmt.Errorf("%s: unsupported window %q", describe, window)
	}
	return w, nil
}

// normaliseKeywordStatus maps Google's AdGroupCriterionStatus onto the closed vocabulary the
// API contract declares, folding anything unrecognised onto StatusUnknown.
//
// This is required rather than cosmetic. The design declares Enum("ENABLED","PAUSED",
// "REMOVED","UNKNOWN") on the response attribute, and Goa generates a matching validator in
// the CLIENT — the server never validates its own response body, so an out-of-enum value is
// not caught here, it makes the generated client reject the ENTIRE response. Google's enum
// also carries UNSPECIFIED/UNKNOWN, and an omitted proto field decodes to "", none of which
// the contract admits.
//
// Folding onto UNKNOWN rather than dropping the row is deliberate and follows this package's
// established reading (see CampaignRef.AdvertisingChannelType): a caller must be able to tell
// "Google said something we don't handle" from "Google said nothing". The row still carries
// its ids and counters, which are what the caller acts on.
func normaliseKeywordStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case StatusEnabled:
		return StatusEnabled
	case StatusPaused:
		return StatusPaused
	case StatusRemoved:
		return StatusRemoved
	default:
		return KeywordStatusUnknown
	}
}

// normaliseKeywordMatchType maps Google's KeywordMatchType onto the closed vocabulary the API
// contract declares. Same reasoning as normaliseKeywordStatus — and the same hazard, since
// KeywordMatchType carries UNSPECIFIED and UNKNOWN of its own.
func normaliseKeywordMatchType(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case MatchTypeExact:
		return MatchTypeExact
	case MatchTypePhrase:
		return MatchTypePhrase
	case MatchTypeBroad:
		return MatchTypeBroad
	default:
		return MatchTypeUnknown
	}
}

// GetKeywordPerformance reads the account's top keywords by impressions over window.
//
// Only ad-group criteria of type KEYWORD are returned, and the status predicate is an
// ALLOW-LIST (ENABLED, PAUSED) rather than an exclusion of REMOVED. The difference matters:
// AdGroupCriterionStatus also carries UNSPECIFIED and UNKNOWN, and an omitted proto field
// decodes to "" — all three survive `!= 'REMOVED'` and would be offered as actionable rows
// carrying a pause/remove action that cannot meaningfully apply. Enumerate the live states
// and default-deny, matching campaignRowIdentity's positive switch.
//
// Status and MatchType are additionally normalised on the way out: the read is confined to the
// project's campaigns but still returns keywords this service never created (a campaign adopted,
// or edited in the Google UI), so an unrecognised upstream value is reachable regardless of what
// the create path restricts itself to. See normaliseKeywordStatus.
//
// The query asks for maxKeywordRows+1 rows so a full page can be told from a truncated
// one; the extra row is dropped and reported as Truncated. Ordering is by impressions
// descending, which is what makes a truncated answer the useful slice rather than an
// arbitrary one.
// campaignScopePredicate renders the GAQL predicate that confines a read to campaignIDs.
//
// It is the security boundary for the two account-wide reads in this file. Google Ads is ONE
// customer shared across every foundation (docs/architecture.md, "Account Tenancy"), so a query
// scoped only by the connection returns every project's keywords, spend and demographics; this
// predicate is what narrows it to the campaigns the calling project actually owns.
//
// It returns an error for an EMPTY list rather than an empty string. An empty string would be
// concatenated into the query and silently restore the account-wide read — the exact defect this
// exists to prevent, reintroduced by the one input most likely to occur (a project that has
// dispatched nothing). Callers must handle "no campaigns" before building a query; making the
// empty case unrepresentable here means a caller cannot forget.
//
// Each id is validated digits-only, matching GetCampaignMetrics: these are concatenated into the
// query string and GAQL has no parameterized queries, so an unvalidated id is an injection vector.
func campaignScopePredicate(campaignIDs []string, op string) (string, error) {
	if len(campaignIDs) == 0 {
		return "", fmt.Errorf("%s: no campaign ids to scope the query to; an unscoped read would "+
			"return every project's data from the shared customer", op)
	}
	// The ids are rendered UNQUOTED, exactly as GetCampaign and GetCampaignMetrics render the
	// same field: campaign.id is an int64 in GAQL, so quoting makes this a string comparison
	// against a numeric column. That is not a cosmetic difference here — a predicate Google
	// rejects or matches nothing would defeat the very scoping this function exists to enforce,
	// which is the security boundary for two otherwise account-wide reads. No escaping question
	// arises: every value has already been proven to be nothing but digits.
	ids := make([]string, 0, len(campaignIDs))
	for _, raw := range campaignIDs {
		id := strings.TrimSpace(raw)
		if !customerIDRE.MatchString(id) {
			return "", fmt.Errorf("%s: campaign id %q must be digits only", op, raw)
		}
		ids = append(ids, id)
	}
	return "campaign.id IN (" + strings.Join(ids, ", ") + ")", nil
}

func (c *Client) GetKeywordPerformance(ctx context.Context, window MetricsWindow, campaignIDs []string) (*KeywordPerformance, error) {
	w, err := resolveWindow(window, "get keyword performance")
	if err != nil {
		return nil, err
	}
	scope, err := campaignScopePredicate(campaignIDs, "get keyword performance")
	if err != nil {
		return nil, err
	}

	// LIMIT is maxKeywordRows+1: one row beyond the cap is the truncation probe. Both
	// values are compile-time constants, never caller input, so neither is an injection
	// vector the way the window and any id would be.
	query := fmt.Sprintf(
		"SELECT ad_group_criterion.criterion_id, ad_group_criterion.status, "+
			"ad_group_criterion.keyword.text, ad_group_criterion.keyword.match_type, "+
			"ad_group.id, campaign.id, "+
			"metrics.impressions, metrics.clicks, metrics.cost_micros "+
			"FROM keyword_view "+
			"WHERE segments.date DURING %s AND ad_group_criterion.status IN ('ENABLED', 'PAUSED') "+
			"AND %s "+
			"ORDER BY metrics.impressions DESC "+
			"LIMIT %d",
		w, scope, maxKeywordRows+1,
	)

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get keyword performance: %w", err)
	}

	truncated := len(rows) > maxKeywordRows
	if truncated {
		rows = rows[:maxKeywordRows]
	}

	out := make([]KeywordRow, 0, len(rows))
	for i, raw := range rows {
		var row gaqlKeywordRow
		if uErr := json.Unmarshal(raw, &row); uErr != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode keyword row at index %d: %w", i, uErr),
			}
		}
		impressions, clicks, costMicros, pErr := parseRowMetrics(row.Metrics, fmt.Sprintf("get keyword performance: row %d", i))
		if pErr != nil {
			return nil, pErr
		}
		// A row whose criterion or ad group id is missing cannot be acted on: the
		// keyword-actions endpoint needs BOTH to address a criterion. Returning it would
		// hand the caller a row whose action button cannot work, so fail loudly instead —
		// an absent id here means the SELECT and this struct have drifted apart.
		if strings.TrimSpace(row.AdGroupCriterion.CriterionID) == "" || strings.TrimSpace(row.AdGroup.ID) == "" {
			return nil, fmt.Errorf("get keyword performance: row %d is missing its criterion or ad group id", i)
		}
		out = append(out, KeywordRow{
			CriterionID: row.AdGroupCriterion.CriterionID,
			AdGroupID:   row.AdGroup.ID,
			CampaignID:  row.Campaign.ID,
			Text:        row.AdGroupCriterion.Keyword.Text,
			MatchType:   normaliseKeywordMatchType(row.AdGroupCriterion.Keyword.MatchType),
			Status:      normaliseKeywordStatus(row.AdGroupCriterion.Status),
			Impressions: impressions,
			Clicks:      clicks,
			CostMicros:  costMicros,
			Ctr:         ctrFor(impressions, clicks),
		})
	}

	return &KeywordPerformance{Window: w, Rows: out, Truncated: truncated}, nil
}

// audienceQuery describes one demographic breakdown: which GAQL resource carries it and
// which field names the bucket.
type audienceQuery struct {
	dimension string
	// selectField is the field naming the bucket (e.g. ad_group_criterion.age_range.type).
	selectField string
	// from is the GAQL resource providing the breakdown.
	from string
	// jsonPath walks the decoded row to the bucket's value.
	jsonPath []string
}

// audienceQueries is the fixed set of breakdowns this client reads. Adding one is a
// deliberate edit here, never a caller-supplied resource name — every component of the
// GAQL string below comes from this table, so none of it is caller input.
var audienceQueries = []audienceQuery{
	{
		dimension:   DimensionAge,
		selectField: "ad_group_criterion.age_range.type",
		from:        "age_range_view",
		jsonPath:    []string{"adGroupCriterion", "ageRange", "type"},
	},
	{
		dimension:   DimensionGender,
		selectField: "ad_group_criterion.gender.type",
		from:        "gender_view",
		jsonPath:    []string{"adGroupCriterion", "gender", "type"},
	},
	{
		dimension: DimensionDevice,
		// Device is a SEGMENT, not a criterion: it segments the campaign resource rather
		// than living on its own view. That is why it selects segments.device FROM campaign
		// while the other two select a criterion field from a *_view.
		selectField: "segments.device",
		from:        "campaign",
		jsonPath:    []string{"segments", "device"},
	},
}

// GetAudienceInsights reads age, gender and device breakdowns for the account over window.
//
// The three breakdowns are three separate GAQL queries. A failure in ANY of them fails the
// whole call rather than returning the breakdowns that loaded: each dimension independently
// covers the same traffic, and a caller shown two of three has no way to tell that the
// third is missing rather than empty — which is how a campaign gets re-targeted on a
// partial demographic picture.
//
// Google's UNDETERMINED/UNKNOWN buckets are returned as-is. They are real unattributed
// traffic, often a large share of it, and dropping them would make the buckets sum to less
// than the campaign's impressions with nothing indicating why.
func (c *Client) GetAudienceInsights(ctx context.Context, window MetricsWindow, campaignIDs []string) (*AudienceInsights, error) {
	w, err := resolveWindow(window, "get audience insights")
	if err != nil {
		return nil, err
	}
	scope, err := campaignScopePredicate(campaignIDs, "get audience insights")
	if err != nil {
		return nil, err
	}

	var buckets []AudienceBucket
	for _, aq := range audienceQueries {
		// Every interpolated value is either a constant from audienceQueries or the
		// allow-listed window — no caller input reaches the query string.
		query := fmt.Sprintf(
			"SELECT %s, metrics.impressions, metrics.clicks, metrics.cost_micros "+
				"FROM %s WHERE segments.date DURING %s AND %s",
			aq.selectField, aq.from, w, scope,
		)
		rows, sErr := c.gaqlSearch(ctx, query)
		if sErr != nil {
			return nil, fmt.Errorf("get audience insights (%s): %w", aq.dimension, sErr)
		}

		// One GAQL row per bucket is the shape for age_range_view and gender_view, but the
		// DEVICE query segments the campaign resource, so it returns one row per
		// (campaign, device) pair — several rows per device across an account with multiple
		// campaigns. Aggregate by bucket value rather than assuming one row each; assuming
		// would silently report only the last campaign's numbers for every device.
		totals := map[string]*AudienceBucket{}
		var order []string
		for i, raw := range rows {
			var generic map[string]json.RawMessage
			if uErr := json.Unmarshal(raw, &generic); uErr != nil {
				return nil, &transportError{
					Method: http.MethodPost,
					Path:   c.customerPath("googleAds:search"),
					Err:    fmt.Errorf("decode %s row at index %d: %w", aq.dimension, i, uErr),
				}
			}
			value, vErr := extractStringPath(generic, aq.jsonPath)
			if vErr != nil {
				return nil, fmt.Errorf("get audience insights (%s): row %d: %w", aq.dimension, i, vErr)
			}
			var m struct {
				Metrics gaqlMetricRowMetrics `json:"metrics"`
			}
			if uErr := json.Unmarshal(raw, &m); uErr != nil {
				return nil, &transportError{
					Method: http.MethodPost,
					Path:   c.customerPath("googleAds:search"),
					Err:    fmt.Errorf("decode %s metrics at index %d: %w", aq.dimension, i, uErr),
				}
			}
			impressions, clicks, costMicros, pErr := parseRowMetrics(m.Metrics, fmt.Sprintf("get audience insights (%s): row %d", aq.dimension, i))
			if pErr != nil {
				return nil, pErr
			}
			b, seen := totals[value]
			if !seen {
				b = &AudienceBucket{Dimension: aq.dimension, Value: value}
				totals[value] = b
				order = append(order, value)
			}
			b.Impressions += impressions
			b.Clicks += clicks
			b.CostMicros += costMicros
		}

		dimBuckets := make([]AudienceBucket, 0, len(order))
		for _, v := range order {
			b := totals[v]
			// Computed AFTER aggregation: summing per-row CTRs would weight a
			// thousand-impression campaign the same as a ten-impression one.
			b.Ctr = ctrFor(b.Impressions, b.Clicks)
			dimBuckets = append(dimBuckets, *b)
		}
		// Stable, meaningful order within the dimension. sort.SliceStable with an
		// impressions-descending comparator and a value tie-break makes the response
		// deterministic for a given upstream answer, which matters for a UI table and for
		// tests that assert on row order.
		sort.SliceStable(dimBuckets, func(i, j int) bool {
			if dimBuckets[i].Impressions != dimBuckets[j].Impressions {
				return dimBuckets[i].Impressions > dimBuckets[j].Impressions
			}
			return dimBuckets[i].Value < dimBuckets[j].Value
		})
		buckets = append(buckets, dimBuckets...)
	}

	return &AudienceInsights{Window: w, Buckets: buckets}, nil
}

// extractStringPath walks a decoded row to a nested string field. A missing or non-string
// value is an error rather than an empty bucket name: an unnamed demographic bucket is not
// a usable row, and silently emitting "" would collide every such row into one.
func extractStringPath(row map[string]json.RawMessage, path []string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("empty field path")
	}
	cur := row
	for i, key := range path {
		raw, ok := cur[key]
		if !ok {
			return "", fmt.Errorf("missing field %q", strings.Join(path[:i+1], "."))
		}
		if i == len(path)-1 {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", fmt.Errorf("field %q is not a string", strings.Join(path, "."))
			}
			if strings.TrimSpace(s) == "" {
				return "", fmt.Errorf("field %q is empty", strings.Join(path, "."))
			}
			return s, nil
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return "", fmt.Errorf("field %q is not an object", strings.Join(path[:i+1], "."))
		}
		cur = next
	}
	return "", fmt.Errorf("missing field %q", strings.Join(path, "."))
}

// KeywordAction is one requested keyword mutation. AdGroupID and CriterionID together
// address the criterion — a criterion id is unique only within its ad group.
type KeywordAction struct {
	AdGroupID   string
	CriterionID string
	Action      string
}

// KeywordActionOutcome is one applied mutation.
type KeywordActionOutcome struct {
	AdGroupID    string `json:"adGroupId"`
	CriterionID  string `json:"criterionId"`
	Action       string `json:"action"`
	ResourceName string `json:"resourceName"`
}

// adGroupCriterionStatusUpdate is the update payload for a PAUSE operation.
type adGroupCriterionStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// keywordMutateOperation is one adGroupCriteria:mutate entry. It carries update+updateMask
// (PAUSE) or remove (REMOVE) — never both.
//
// This is a separate type from mutateOperation because that one has no `remove` field:
// every existing caller creates or updates. Adding `remove` to the shared type would put an
// omitempty field on every create payload in the package for the benefit of this one path.
type keywordMutateOperation struct {
	Update     *adGroupCriterionStatusUpdate `json:"update,omitempty"`
	UpdateMask string                        `json:"updateMask,omitempty"`
	Remove     string                        `json:"remove,omitempty"`
}

// keywordMutateRequest is the POST body for the keyword-actions batch.
//
// partialFailure is deliberately NOT set (Google defaults it false), which makes the batch
// atomic: one rejected operation rolls the whole thing back. That is the behaviour the API
// contract promises. Enabling partial failure would let a caller pausing eight keywords to
// stop a budget leak be told five were paused, leaving them to work out which three still
// spend before they can act again.
type keywordMutateRequest struct {
	Operations []keywordMutateOperation `json:"operations"`
}

// ValidateKeywordActions checks a batch WITHOUT contacting Google.
//
// Split out from ApplyKeywordActions so the service layer can reject a bad batch before it
// resolves credentials or touches the platform at all — the same "validate before any
// mutating upstream call" ordering the create path uses. Every rule here is a permanent
// input fault: no amount of retrying makes a malformed criterion id valid.
func ValidateKeywordActions(actions []KeywordAction) ([]KeywordAction, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("google-ads: at least one keyword action is required")
	}
	if len(actions) > maxKeywordActions {
		return nil, fmt.Errorf("google-ads: at most %d keyword actions are supported, got %d", maxKeywordActions, len(actions))
	}
	out := make([]KeywordAction, 0, len(actions))
	seen := map[string]struct{}{}
	for i, a := range actions {
		adGroupID := strings.TrimSpace(a.AdGroupID)
		criterionID := strings.TrimSpace(a.CriterionID)
		// Digits-only, for the same reason customerIDRE guards the metrics query: both ids
		// are concatenated into a resource name that is sent upstream.
		if !customerIDRE.MatchString(adGroupID) {
			return nil, fmt.Errorf("google-ads: keyword action %d has an ad group id that is not digits only", i)
		}
		if !customerIDRE.MatchString(criterionID) {
			return nil, fmt.Errorf("google-ads: keyword action %d has a criterion id that is not digits only", i)
		}
		action := strings.ToUpper(strings.TrimSpace(a.Action))
		switch action {
		case KeywordActionPause, KeywordActionRemove:
		default:
			return nil, fmt.Errorf("google-ads: keyword action %d has unsupported action %q (want %s or %s)",
				i, a.Action, KeywordActionPause, KeywordActionRemove)
		}
		// A batch naming the same criterion twice is refused rather than de-duplicated.
		// Unlike validateKeywords' create-path dedupe, two entries here can carry DIFFERENT
		// actions (pause and remove), and there is no defensible way to pick one — silently
		// dropping either applies a mutation the caller did not ask for.
		key := adGroupID + "~" + criterionID
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads: keyword action %d addresses criterion %s more than once in one batch", i, key)
		}
		seen[key] = struct{}{}
		out = append(out, KeywordAction{AdGroupID: adGroupID, CriterionID: criterionID, Action: action})
	}
	return out, nil
}

// ApplyKeywordActions pauses or removes existing ad-group criteria in ONE atomic mutate.
//
// The batch either wholly applies or wholly fails — see keywordMutateRequest. Every action
// is validated before the request is built, so a malformed batch never reaches Google.
//
// A 2xx whose result count does not match the operation count is reported as UNCONFIRMED
// rather than success: the mutations may have been applied, and this path MUTATES SPEND, so
// a caller must verify in Google Ads before retrying rather than assume nothing happened.
func (c *Client) ApplyKeywordActions(ctx context.Context, actions []KeywordAction) ([]KeywordActionOutcome, error) {
	validated, err := ValidateKeywordActions(actions)
	if err != nil {
		return nil, err
	}

	// Refuse before the request exists when the context is already done, matching
	// createAdGroupTargeting: a cancelled context that still issues the mutate would leave
	// the caller unable to tell whether their keywords were paused.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads keyword actions aborted before any request (context already done; no keyword was changed): %w", ctxErr)
	}

	ops := make([]keywordMutateOperation, 0, len(validated))
	for _, a := range validated {
		resourceName := fmt.Sprintf("customers/%s/adGroupCriteria/%s~%s", c.account.CustomerID, a.AdGroupID, a.CriterionID)
		switch a.Action {
		case KeywordActionPause:
			ops = append(ops, keywordMutateOperation{
				Update:     &adGroupCriterionStatusUpdate{ResourceName: resourceName, Status: StatusPaused},
				UpdateMask: "status",
			})
		case KeywordActionRemove:
			ops = append(ops, keywordMutateOperation{Remove: resourceName})
		}
	}

	resp, mErr := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), keywordMutateRequest{Operations: ops}, false)
	if mErr != nil {
		if createOutcomeAmbiguous(mErr) {
			return nil, fmt.Errorf("google-ads keyword actions UNCONFIRMED (%d operation(s); the changes may have been applied — verify in Google Ads before retrying): %w", len(ops), mErr)
		}
		return nil, fmt.Errorf("google-ads keyword actions failed (%d operation(s); no change confirmed): %w", len(ops), mErr)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads keyword actions UNCONFIRMED (%d operation(s); 2xx with a malformed/short mutate response — the changes may have been applied — verify in Google Ads before retrying)", len(ops))
	}

	outcomes := make([]KeywordActionOutcome, 0, len(validated))
	for i, r := range mr.Results {
		// Verify the returned resource name addresses the criterion this operation named.
		// adGroupCriterionID rejects a name whose customer id is not this client's, so a
		// substituted or cross-account response cannot be reported as a successful mutation.
		returnedAdGroupID, returnedCriterionID := c.adGroupCriterionID(r.ResourceName)
		if returnedAdGroupID != validated[i].AdGroupID || returnedCriterionID != validated[i].CriterionID {
			return nil, fmt.Errorf("google-ads keyword actions UNCONFIRMED (result %d names criterion %q, which is not the %s~%s this operation addressed — verify in Google Ads before retrying)",
				i, r.ResourceName, validated[i].AdGroupID, validated[i].CriterionID)
		}
		outcomes = append(outcomes, KeywordActionOutcome{
			AdGroupID:    validated[i].AdGroupID,
			CriterionID:  validated[i].CriterionID,
			Action:       validated[i].Action,
			ResourceName: r.ResourceName,
		})
	}
	return outcomes, nil
}
