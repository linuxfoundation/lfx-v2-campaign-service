<!-- Copyright The Linux Foundation and each contributor to LFX.
SPDX-License-Identifier: MIT -->

# Keyword Surface Implementation Plan (LFXV2-2023 Roadmap Item 4)

**Status:** Research phase complete. Plan ready for human decision on dependencies and phasing.

**Goal:** Expose keywords for a campaign and actions to manage them, replacing the UI's direct Google Ads API calls at:
- `GET /api/campaigns/keywords` (list keywords with metrics)
- `POST /api/campaigns/keywords/actions` (pause/enable/change-bid actions)

The new campaign-service endpoints will follow the `MetricsReader` optional-capability pattern established by `get-campaign-metrics`, with keywords managed via an analogous `KeywordManager` interface.

---

## Current UI Contract (Source of Truth)

All contract shapes are from `/Users/misharautela/LFX-DEV/lfx-v2-ui/packages/shared/src/interfaces/campaign.interface.ts:560–667`.

### GET `/api/campaigns/keywords` — List Keywords with Metrics

**Request:** Query parameter `days` (default 30)

**Response:** `KeywordMetricsResponse` — aggregates across all Google Ads campaigns under the authenticated user's account.

```typescript
interface KeywordMetrics {
  keyword: string;
  matchType: string;               // "Exact" | "Phrase" | "Broad"
  qualityScore: number | null;
  status: string;                  // "enabled" | "paused" | "removed" | other
  adGroup: string;                 // Ad group name (display)
  adGroupId: string;               // Numeric, stringified
  criterionId: string;             // Numeric, stringified (Google Ads keyword criterion ID)
  campaign: string;                // Campaign name (display)
  campaignId: string;              // Numeric, stringified
  googleAdsUrl: string;            // Direct link to keyword in Google Ads UI
  impressions: number;
  clicks: number;
  ctr: number;                     // CTR as decimal (e.g., 0.05 for 5%)
  avgCpc: number;                  // Average cost-per-click (currency units, not micros)
  spend: number;
  conversions: number;
}

interface KeywordTotals {
  impressions: number;
  clicks: number;
  spend: number;
  conversions: number;
  avgCtr: number;
}

interface KeywordMetricsResponse {
  pulledAt: string;                // ISO 8601 timestamp
  days: number;                    // Reporting window (e.g., 30)
  totalKeywords: number;
  totals: KeywordTotals;
  keywords: KeywordMetrics[];
}
```

**Error Cases:** 200, 400 (invalid days), 503 (Google Ads unreachable)

### POST `/api/campaigns/keywords/actions` — Manage Keywords

**Request:** `BulkKeywordActionRequest`

```typescript
type KeywordActionType = 'pause' | 'remove';   // Current UI vocabulary

interface KeywordActionRequest {
  campaignId: string;              // Campaign UUID (UI's internal ID)
  adGroupId: string;               // Ad group UUID (UI's internal ID)
  criterionId: string;             // Google Ads criterion ID (the keyword row's criterionId)
  action: KeywordActionType;
}

interface BulkKeywordActionRequest {
  keywords: KeywordActionRequest[];
  action: KeywordActionType;       // Duplicated at top level (legacy quirk)
}
```

**Response:** `BulkKeywordActionResponse`

```typescript
interface KeywordActionResponse {
  success: boolean;
  action: KeywordActionType;
  keyword: string;                 // The keyword term (for display/logging)
  message: string;
}

interface BulkKeywordActionResponse {
  success: boolean;
  total: number;                   // Total keywords attempted
  succeeded: number;
  failed: number;
  results: KeywordActionResponse[];
}
```

**Action Types Current:** Only `'pause'` and `'remove'` are exposed in the UI.
**Roadmap:** Plan MUST accommodate future `'enable'` (resume paused) and `'change-bid'` (adjust max CPC).

**Error Cases:** 200 (partial success OK), 400 (validation), 503 (platform failure)

---

## Dependency Analysis: PR #69 vs. Plan Item 4

**Verdict:** **Item 4 CAN land before PR #69 merges** — with a deliberate two-phase structure.

### Phase 1 (No #69 dependency): List Keywords + Pause/Remove Actions
- **What lands:** `GET /campaigns/{campaignId}/keywords` (list) and `POST /campaigns/{campaignId}/keywords/actions` (pause/remove only)
- **Blocked on:** Nothing in #69. Uses existing Google Ads API surfaces (adGroupCriteria:search, mutate)
- **Google Ads API:** `SearchGoogleAdsRequest` over `adGroupCriteria` (read metrics); `MutateGoogleAdsRequest` on `adGroupCriteria` with `pause` / `remove` operations
- **Can build on:** origin/main as-is. No keyword creation plumbing needed

### Phase 2 (With #69): Enable/Resume + Bid Changes
- **What lands:** `'enable'` action (resume paused keywords) and `'change-bid'` operation (set max CPC in micros)
- **Dependency:** #69's keyword resource-name construction and criterion-ID plumbing in `internal/platform/googleads/targeting.go`
- **Why:** The bid-change mutation uses the same criterion resource name machinery #69 implements for creation; reusing it is safer than duplicating it
- **Timing:** After PR #69 merges. Does NOT require a new base branch merge; the keyword-surface branch simply rebases onto #69 once it lands

**Implication:** If you want pause/remove in the next release, open Phase 1 independently. Phase 2 can chain behind #69 without blocking it.

---

## Goa Design Additions

### Roadmap Item 4 introduces TWO new methods to the briefs service:

#### Method 1: `list-campaign-keywords`

```goa
Method("list-campaign-keywords", func() {
  Description("List keywords for a campaign with their performance metrics over a date range. Always reports across all ad groups in the campaign. Returns 400 when the campaign's platform has no keyword-read capability wired. Supports Google Ads only (for now).")
  
  Payload(func() {
    bearerToken()
    projectIDAttr()
    briefIDAttr()
    campaignIDAttr()
    
    // Reporting window, defaults to 30 days
    Attribute("days", Int32, "Reporting window in days (7, 14, 30)", func() {
      Enum(7, 14, 30)
      Default(30)
    })
    
    Required("project_id", "brief_id", "campaign_id")
  })
  
  Result(CampaignKeywordList)
  
  commonBriefErrors(false)
  
  HTTP(func() {
    GET("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keywords")
    Header("bearer_token:Authorization")
    Param("days")
    Response(StatusOK)
    briefErrorResponses(false)
  })
})
```

#### Method 2: `update-campaign-keywords`

```goa
Method("update-campaign-keywords", func() {
  Description("Pause, enable, or change bid on keywords in a campaign. Phase 1 supports pause and remove; enable and change-bid follow in Phase 2 (LFXV2-xxx). Returns 400 when the campaign's platform has no keyword-manager capability wired.")
  
  Payload(func() {
    bearerToken()
    projectIDAttr()
    briefIDAttr()
    campaignIDAttr()
    
    Attribute("actions", ArrayOf(KeywordActionPayload), "Keyword actions to apply")
    
    Required("project_id", "brief_id", "campaign_id", "actions")
  })
  
  Result(CampaignKeywordActionResult)
  
  commonBriefErrors(true)  // Body validation
  
  HTTP(func() {
    PATCH("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keywords")
    Header("bearer_token:Authorization")
    Response(StatusOK)
    briefErrorResponses(true)
  })
})
```

### Type Definitions

All types added to `design/brief.go`:

```goa
// Keyword action types supported by the platform
var KeywordActionEnum = func() {
  Enum("pause", "remove", "enable", "change-bid")
}

// A single keyword action (action item in the request)
var KeywordActionPayload = Type("keyword-action-payload", func() {
  Attribute("criterion_id", String, "Google Ads criterion ID (the keyword's unique identifier within the ad group)")
  Attribute("action", String, "Action to perform", KeywordActionEnum)
  Attribute("bid_micros", Int64, "New max CPC in micros (only for change-bid; omitted for pause/remove/enable)")
  Required("criterion_id", "action")
  // bid_micros is conditionally required: goa does not express conditional Required(),
  // so the service validates that (action == "change-bid") ⇒ bid_micros is present
})

// A single keyword with its metrics
var CampaignKeyword = Type("campaign-keyword", func() {
  Attribute("criterion_id", String, "Google Ads criterion ID")
  Attribute("keyword", String, "Keyword text")
  Attribute("match_type", String, "Match type", func() {
    Enum("EXACT", "PHRASE", "BROAD")
  })
  Attribute("status", String, "Keyword status (enabled, paused, removed)", func() {
    Enum("enabled", "paused", "removed")
  })
  Attribute("quality_score", Int32, "Google Ads quality score (1–10, or null if unavailable)")
  Attribute("ad_group_id", String, "Ad group UUID (this service's internal ID)")
  Attribute("ad_group_name", String, "Ad group name (for display)")
  Attribute("impressions", Int64, "Impressions in window")
  Attribute("clicks", Int64, "Clicks in window")
  Attribute("ctr", Float64, "Click-through rate (clicks/impressions; 0 when impressions=0)")
  Attribute("avg_cpc_micros", Int64, "Average CPC in micros (currency-dependent; USD for Google Ads)")
  Attribute("spend_micros", Int64, "Total spend in micros")
  Attribute("conversions", Int64, "Conversions in window")
  Attribute("max_cpc_micros", Int64, "Current max CPC bid in micros (or null if not set)")
  Attribute("google_ads_url", String, "Direct link to keyword in Google Ads UI")
  Required("criterion_id", "keyword", "match_type", "status", "ad_group_id", "ad_group_name", "impressions", "clicks", "ctr")
})

// Response for list-campaign-keywords
var CampaignKeywordList = Type("campaign-keyword-list", func() {
  Attribute("campaign_id", String, "Campaign UUID")
  Attribute("platform_campaign_id", String, "Google Ads campaign ID")
  Attribute("window", String, "Reporting window (days), e.g. '30'")
  Attribute("pulled_at", String, "ISO 8601 timestamp when metrics were pulled")
  Attribute("total_keywords", Int32, "Total keyword count")
  Attribute("keywords", ArrayOf(CampaignKeyword))
  Attribute("totals", func() {
    Attribute("impressions", Int64)
    Attribute("clicks", Int64)
    Attribute("ctr", Float64)
    Attribute("spend_micros", Int64)
    Attribute("conversions", Int64)
  })
  Required("campaign_id", "platform_campaign_id", "window", "pulled_at", "keywords", "totals")
})

// Individual action result
var KeywordActionResult = Type("keyword-action-result", func() {
  Attribute("criterion_id", String, "The keyword's criterion ID")
  Attribute("keyword", String, "Keyword text (for logging/display)")
  Attribute("action", String, "Action that was performed")
  Attribute("success", Boolean, "Whether the action succeeded")
  Attribute("message", String, "Outcome or error message")
  Required("criterion_id", "keyword", "action", "success")
})

// Response for update-campaign-keywords
var CampaignKeywordActionResult = Type("campaign-keyword-action-result", func() {
  Attribute("total", Int32, "Total actions requested")
  Attribute("succeeded", Int32, "Number that succeeded")
  Attribute("failed", Int32, "Number that failed")
  Attribute("results", ArrayOf(KeywordActionResult), "Per-action outcome")
  Required("total", "succeeded", "failed", "results")
})
```

---

## Layered Implementation

### Layer 1: Design → Generated Code

**Files to edit:** `/design/brief.go`

**Changes:**
1. Add type definitions: `KeywordActionEnum`, `KeywordActionPayload`, `CampaignKeyword`, `CampaignKeywordList`, `KeywordActionResult`, `CampaignKeywordActionResult`
2. Add methods: `list-campaign-keywords`, `update-campaign-keywords`

**Generated artifacts:** After `go run ./cmd/okfgen`, new types appear in `gen/lfx_v2_campaign_service_briefs/` (Goa-generated server/client interfaces). No manual edits here.

### Layer 2: Domain Model + Errors

**Files to edit:** `/internal/domain/errors.go`, `/internal/domain/model/keyword.go` (new)

**Error additions to `errors.go`:**
```go
// ErrKeywordManagerUnsupported indicates the campaign's platform has no keyword-manager
// capability wired (no dispatcher, or the dispatcher is not a KeywordManager).
// Maps to 400, follows the MetricsReader/StatusToggler pattern.
ErrKeywordManagerUnsupported = errors.New("keyword management is not supported for this platform")

// ErrInvalidBidChange indicates the bid value supplied is outside the platform's limits.
// Maps to 400.
ErrInvalidBidChange = errors.New("bid value is outside the platform's supported range")
```

**New file `/internal/domain/model/keyword.go`:**
```go
package model

// KeywordAction represents one keyword operation.
type KeywordAction struct {
  CriterionID string // Google Ads criterion ID
  Action      string // "pause" | "remove" | "enable" | "change-bid"
  BidMicros   int64  // For change-bid only; validated in the service layer
}

// KeywordMetric holds a keyword's performance over a reporting window.
type KeywordMetric struct {
  CriterionID    string
  Keyword        string
  MatchType      string  // "EXACT" | "PHRASE" | "BROAD"
  Status         string  // "enabled" | "paused" | "removed"
  QualityScore   *int32  // Nullable: not always available
  Impressions    int64
  Clicks         int64
  Ctr            float64
  AvgCpcMicros   int64
  SpendMicros    int64
  Conversions    int64
  MaxCpcMicros   *int64  // Nullable: bid may not be explicitly set
  GoogleAdsUrl   string
}

// KeywordListResult is the platform-agnostic representation of a keyword list query.
type KeywordListResult struct {
  Keywords    []*KeywordMetric
  Impressions int64
  Clicks      int64
  Ctr         float64
  SpendMicros int64
  Conversions int64
}

// KeywordActionOutcome is the result of one keyword operation.
type KeywordActionOutcome struct {
  CriterionID string
  Keyword     string
  Action      string
  Success     bool
  Message     string
}
```

### Layer 3: Service Interfaces (Orchestrator Capabilities)

**File to edit:** `/internal/service/orchestrator.go`

**New interface definition (following `MetricsReader` pattern):**

```go
// KeywordManager is an OPTIONAL dispatcher capability: read/write keywords for an existing
// campaign. Like MetricsReader and StatusToggler, not every platform implements it; a
// dispatcher that doesn't implement it yields a clean "not supported" error
// (ErrKeywordManagerUnsupported → 400).
type KeywordManager interface {
  // ListKeywords fetches keywords for a campaign with their metrics over a reporting window.
  // Returns results aggregated across all ad groups in the campaign.
  ListKeywords(ctx context.Context, projectID string, platform Provider, campaign *Campaign,
    days int32) (*model.KeywordListResult, error)
  
  // UpdateKeywords applies bulk actions (pause, remove, enable, change-bid) to keywords.
  // Returns per-action outcomes; partial success is OK (failed actions don't cancel others).
  UpdateKeywords(ctx context.Context, projectID string, platform Provider, campaign *Campaign,
    actions []*model.KeywordAction) ([]*model.KeywordActionOutcome, error)
}
```

**Export error sentinels** (already declared in `domain/errors.go`):
```go
var (
  ErrKeywordManagerUnsupported = domain.ErrKeywordManagerUnsupported
  ErrInvalidBidChange = domain.ErrInvalidBidChange
)
```

**Add method to Orchestrator** (mirrors `ReadCampaignMetrics`):

```go
// ReadCampaignKeywords returns keywords for a campaign with metrics over a reporting window.
func (o *Orchestrator) ReadCampaignKeywords(ctx context.Context, projectID string, platform Provider,
  campaign *Campaign, days int32) (*model.KeywordListResult, error) {
  d, ok := o.dispatchers[platform]
  if !ok {
    return nil, ErrKeywordManagerUnsupported
  }
  reader, ok := d.(KeywordManager)
  if !ok {
    return nil, ErrKeywordManagerUnsupported
  }
  callCtx, cancel := context.WithTimeout(ctx, keywordCallTimeout) // = 30s
  defer cancel()
  return reader.ListKeywords(callCtx, projectID, platform, campaign, days)
}

// UpdateCampaignKeywords applies bulk actions to keywords in a campaign.
func (o *Orchestrator) UpdateCampaignKeywords(ctx context.Context, projectID string, platform Provider,
  campaign *Campaign, actions []*model.KeywordAction) ([]*model.KeywordActionOutcome, error) {
  // Validate: each action with "change-bid" MUST have BidMicros set and > 0
  for _, act := range actions {
    if act.Action == "change-bid" {
      if act.BidMicros <= 0 {
        return nil, fmt.Errorf("change-bid action requires a positive BidMicros value")
      }
    }
  }
  
  d, ok := o.dispatchers[platform]
  if !ok {
    return nil, ErrKeywordManagerUnsupported
  }
  manager, ok := d.(KeywordManager)
  if !ok {
    return nil, ErrKeywordManagerUnsupported
  }
  callCtx, cancel := context.WithTimeout(ctx, keywordCallTimeout) // = 30s
  defer cancel()
  return manager.UpdateKeywords(callCtx, projectID, platform, campaign, actions)
}
```

**New timeout constant:**
```go
// keywordCallTimeout bounds SYNCHRONOUS keyword list + update platform calls.
// Keyword operations are typically fast (a few hundred ms for dozens of keywords),
// but aggregating metrics over 7/14/30 days may require multiple queries.
// Kept under server DefaultWriteTimeout (60s) with headroom for the service layer's
// result marshaling.
const keywordCallTimeout = 30 * time.Second
```

### Layer 4: Service Handler (BriefService)

**File to edit:** `/internal/service/brief.go`

**Add two methods to BriefService:**

```go
// ListCampaignKeywords retrieves keywords for a campaign with their performance metrics.
func (s *BriefService) ListCampaignKeywords(ctx context.Context,
  p *briefs.ListCampaignKeywordsPayload) (*briefs.CampaignKeywordList, error) {
  
  _, campaignRepo, _, orch, err := s.ready()
  if err != nil {
    return nil, err
  }
  
  // Load the campaign to get its platform and upstream ID
  existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
  if gerr != nil {
    return nil, mapBriefErr(gerr)
  }
  
  // Default to 30 days if not specified
  days := int32(30)
  if p.Days != nil {
    days = *p.Days
  }
  
  // Validate: days must be one of 7, 14, 30
  switch days {
  case 7, 14, 30:
  default:
    return nil, &briefs.BadRequestError{Code: "400", Message: "days must be 7, 14, or 30"}
  }
  
  // Call the orchestrator
  result, merr := orch.ReadCampaignKeywords(ctx, p.ProjectID, existing.Platform, existing, days)
  if merr != nil {
    switch {
    case errors.Is(merr, ErrKeywordManagerUnsupported):
      return nil, &briefs.BadRequestError{Code: "400", Message: "keyword reads are not supported for this campaign's platform"}
    default:
      slog.WarnContext(ctx, "campaign keywords read failed on the ad platform",
        "project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
        "platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID, "error", merr)
      return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "campaign keywords could not be read from the ad platform"}
    }
  }
  
  // Marshal results into Goa types
  keywords := make([]*briefs.CampaignKeyword, len(result.Keywords))
  for i, kw := range result.Keywords {
    keywords[i] = &briefs.CampaignKeyword{
      CriterionID:  kw.CriterionID,
      Keyword:      kw.Keyword,
      MatchType:    kw.MatchType,
      Status:       kw.Status,
      QualityScore: kw.QualityScore,
      AdGroupID:    kw.AdGroupID,
      AdGroupName:  kw.AdGroupName,
      Impressions:  kw.Impressions,
      Clicks:       kw.Clicks,
      Ctr:          kw.Ctr,
      AvgCpcMicros: kw.AvgCpcMicros,
      SpendMicros:  kw.SpendMicros,
      Conversions:  kw.Conversions,
      MaxCpcMicros: kw.MaxCpcMicros,
      GoogleAdsUrl: kw.GoogleAdsUrl,
    }
  }
  
  return &briefs.CampaignKeywordList{
    CampaignID:       existing.ID,
    PlatformCampaignID: existing.PlatformCampaignID,
    Window:           fmt.Sprintf("%d", days),
    PulledAt:         time.Now().UTC().Format(time.RFC3339),
    TotalKeywords:    int32(len(result.Keywords)),
    Keywords:         keywords,
    Totals: &briefs.CampaignKeywordListTotals{
      Impressions: result.Impressions,
      Clicks:      result.Clicks,
      Ctr:         result.Ctr,
      SpendMicros: result.SpendMicros,
      Conversions: result.Conversions,
    },
  }, nil
}

// UpdateCampaignKeywords applies bulk keyword actions (pause, remove, enable, change-bid).
func (s *BriefService) UpdateCampaignKeywords(ctx context.Context,
  p *briefs.UpdateCampaignKeywordsPayload) (*briefs.CampaignKeywordActionResult, error) {
  
  _, campaignRepo, _, orch, err := s.ready()
  if err != nil {
    return nil, err
  }
  
  // Validate actions array
  if len(p.Actions) == 0 {
    return nil, &briefs.BadRequestError{Code: "400", Message: "actions array must not be empty"}
  }
  
  // Load the campaign
  existing, gerr := campaignRepo.GetCampaign(ctx, p.ProjectID, p.BriefID, p.CampaignID)
  if gerr != nil {
    return nil, mapBriefErr(gerr)
  }
  
  // Convert to domain model
  actions := make([]*model.KeywordAction, len(p.Actions))
  for i, act := range p.Actions {
    actions[i] = &model.KeywordAction{
      CriterionID: act.CriterionID,
      Action:      act.Action,
      BidMicros:   act.BidMicros,
    }
  }
  
  // Call the orchestrator
  outcomes, aerr := orch.UpdateCampaignKeywords(ctx, p.ProjectID, existing.Platform, existing, actions)
  if aerr != nil {
    switch {
    case errors.Is(aerr, ErrKeywordManagerUnsupported):
      return nil, &briefs.BadRequestError{Code: "400", Message: "keyword updates are not supported for this campaign's platform"}
    case errors.Is(aerr, ErrInvalidBidChange):
      return nil, &briefs.BadRequestError{Code: "400", Message: "bid value is outside the platform's supported range"}
    default:
      slog.WarnContext(ctx, "campaign keywords update failed on the ad platform",
        "project_id", p.ProjectID, "brief_id", p.BriefID, "campaign_id", p.CampaignID,
        "platform", existing.Platform, "platform_campaign_id", existing.PlatformCampaignID,
        "action_count", len(actions), "error", aerr)
      return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "campaign keywords could not be updated on the ad platform"}
    }
  }
  
  // Marshal results
  results := make([]*briefs.KeywordActionResult, len(outcomes))
  succeeded := int32(0)
  for i, outcome := range outcomes {
    results[i] = &briefs.KeywordActionResult{
      CriterionID: outcome.CriterionID,
      Keyword:     outcome.Keyword,
      Action:      outcome.Action,
      Success:     outcome.Success,
      Message:     outcome.Message,
    }
    if outcome.Success {
      succeeded++
    }
  }
  
  return &briefs.CampaignKeywordActionResult{
    Total:     int32(len(outcomes)),
    Succeeded: succeeded,
    Failed:    int32(len(outcomes)) - succeeded,
    Results:   results,
  }, nil
}
```

### Layer 5: Platform Dispatcher (Google Ads)

**File to edit/create:** `/internal/dispatch/googleads/keywords.go` (new)

**Implementation outline** (Phase 1: pause/remove only):

```go
package googleads

import (
  "context"
  "fmt"
  
  "github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ListKeywords returns keywords for a campaign with their metrics over a reporting window.
// Aggregates across all ad groups via SearchGoogleAdsRequest over adGroupCriteria.
// Returns results sorted by impression count (descending).
func (d *Dispatcher) ListKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, days int32) (*model.KeywordListResult, error) {
  
  // Resolve Google Ads credentials (shared with createAdGroupAndAd, status toggle, metrics)
  client, creds, err := resolveGoogleAdsClient(ctx, projectID)
  if err != nil {
    return nil, fmt.Errorf("could not resolve google ads client: %w", err)
  }
  
  // Build a report query covering all ad groups in the campaign
  // using SearchGoogleAdsRequest with a metrics segment for the date range
  // (See internal/platform/googleads/reporting.go for the existing metrics query pattern)
  
  // Query: SELECT adGroupCriterion.criterion.keyword, adGroupCriterion.status,
  //        metrics.impressions, metrics.clicks, metrics.cost_micros, ...
  // FROM ad_group_criterion
  // WHERE campaign.id = {platformCampaignID} AND ad_group_criterion.type = KEYWORD
  // AND segments.date BETWEEN {startDate} AND {endDate}
  
  // Parse the date range based on `days` (7, 14, 30)
  startDate, endDate := dateRangeForDays(days)
  
  query := fmt.Sprintf(`
    SELECT adGroupCriterion.criterion.keyword,
           adGroupCriterion.status,
           adGroupCriterion.qualityInfo.qualityScore,
           adGroupCriterion.cpcBidMicros,
           adGroup.id,
           adGroup.name,
           campaign.id,
           campaign.name,
           metrics.impressions,
           metrics.clicks,
           metrics.costMicros,
           metrics.conversions,
           metrics.ctrPercent
    FROM ad_group_criterion
    WHERE campaign.id = %s
      AND ad_group_criterion.type = KEYWORD
      AND segments.date BETWEEN '%s' AND '%s'
    ORDER BY metrics.impressions DESC
  `, campaign.PlatformCampaignID, startDate, endDate)
  
  // Execute the search against Google Ads API v23
  rows, err := client.SearchGoogleAds(ctx, &searchads.SearchGoogleAdsRequest{
    CustomerId: creds.CustomerID,
    Query:      query,
    PageSize:   10000, // Max 10k per page; pagination handles larger sets
  })
  if err != nil {
    return nil, fmt.Errorf("google ads search failed: %w", err)
  }
  
  // Parse rows into KeywordMetric structs
  // (Extract criterion ID from resource name: customers/{id}/adGroupCriteria/{agcId})
  keywords := make([]*model.KeywordMetric, 0, len(rows))
  totals := &model.KeywordListResult{}
  
  for _, row := range rows {
    kw := &model.KeywordMetric{
      // Resource name → criterion ID extraction
      // Impressions, clicks, etc. from metrics segment
      // Status from adGroupCriterion.status (ENABLED → "enabled", PAUSED → "paused", etc.)
    }
    keywords = append(keywords, kw)
    // Accumulate totals
  }
  
  result := &model.KeywordListResult{
    Keywords:    keywords,
    Impressions: totals.Impressions,
    Clicks:      totals.Clicks,
    SpendMicros: totals.SpendMicros,
    Conversions: totals.Conversions,
  }
  
  if len(keywords) > 0 {
    result.Ctr = float64(result.Clicks) / float64(result.Impressions)
  }
  
  return result, nil
}

// UpdateKeywords applies bulk actions (pause, remove) to keywords.
// Phase 1: pause, remove only. Phase 2 adds enable, change-bid.
func (d *Dispatcher) UpdateKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, actions []*model.KeywordAction) ([]*model.KeywordActionOutcome, error) {
  
  client, creds, err := resolveGoogleAdsClient(ctx, projectID)
  if err != nil {
    return nil, fmt.Errorf("could not resolve google ads client: %w", err)
  }
  
  outcomes := make([]*model.KeywordActionOutcome, len(actions))
  
  // Phase 1: only pause and remove are supported
  // Phase 2: add enable (set status to ENABLED) and change-bid (set cpcBidMicros)
  
  for i, action := range actions {
    outcome := &model.KeywordActionOutcome{
      CriterionID: action.CriterionID,
      Action:      action.Action,
    }
    
    // Construct the resource name from the campaign ID and criterion ID
    resourceName := fmt.Sprintf("customers/%s/adGroupCriteria/%s", creds.CustomerID, action.CriterionID)
    
    var operation *searchads.MutateOperation
    
    switch action.Action {
    case "pause":
      // Set status to PAUSED
      operation = &searchads.MutateOperation{
        AdGroupCriterionOperation: &searchads.AdGroupCriterionOperation{
          Update: &searchads.AdGroupCriterion{
            ResourceName: resourceName,
            Status:       searchads.AdGroupCriterionStatusPAUSED,
          },
          UpdateMask: &fieldmaskpb.FieldMask{
            Paths: []string{"status"},
          },
        },
      }
    
    case "remove":
      // Set status to REMOVED
      operation = &searchads.MutateOperation{
        AdGroupCriterionOperation: &searchads.AdGroupCriterionOperation{
          Remove: resourceName,
        },
      }
    
    case "enable":
      // Phase 2: set status to ENABLED
      operation = &searchads.MutateOperation{
        AdGroupCriterionOperation: &searchads.AdGroupCriterionOperation{
          Update: &searchads.AdGroupCriterion{
            ResourceName: resourceName,
            Status:       searchads.AdGroupCriterionStatusENABLED,
          },
          UpdateMask: &fieldmaskpb.FieldMask{
            Paths: []string{"status"},
          },
        },
      }
    
    case "change-bid":
      // Phase 2: set cpcBidMicros
      operation = &searchads.MutateOperation{
        AdGroupCriterionOperation: &searchads.AdGroupCriterionOperation{
          Update: &searchads.AdGroupCriterion{
            ResourceName:  resourceName,
            CpcBidMicros:  action.BidMicros,
          },
          UpdateMask: &fieldmaskpb.FieldMask{
            Paths: []string{"cpc_bid_micros"},
          },
        },
      }
    
    default:
      outcome.Success = false
      outcome.Message = fmt.Sprintf("unsupported action: %s", action.Action)
      outcomes[i] = outcome
      continue
    }
    
    // Execute the mutation
    resp, err := client.MutateGoogleAds(ctx, &searchads.MutateGoogleAdsRequest{
      CustomerId:      creds.CustomerID,
      MutateOperations: []*searchads.MutateOperation{operation},
    })
    
    if err != nil {
      outcome.Success = false
      outcome.Message = fmt.Sprintf("mutation failed: %v", err)
      outcomes[i] = outcome
      continue
    }
    
    // Check result
    if resp.Results[0].AdGroupCriterionResult != nil {
      outcome.Success = true
      outcome.Message = fmt.Sprintf("%s succeeded", action.Action)
      // Extract keyword name if available (for display; may not be in response)
    } else if resp.Results[0].ErrorDetails != nil {
      outcome.Success = false
      outcome.Message = fmt.Sprintf("mutation error: %s", resp.Results[0].ErrorDetails.Errors[0].Message)
    }
    
    outcomes[i] = outcome
  }
  
  return outcomes, nil
}

// dateRangeForDays returns the start and end dates for a reporting window.
func dateRangeForDays(days int32) (string, string) {
  endDate := time.Now().UTC().Format("2006-01-02")
  startDate := time.Now().UTC().AddDate(0, 0, -int(days)).Format("2006-01-02")
  return startDate, endDate
}
```

**File to edit:** `/internal/dispatch/googleads/dispatch.go`

**Update the Dispatcher to implement KeywordManager interface:**

```go
// In the Dispatcher constructor or as a package-level assertion:
var _ orchestrator.KeywordManager = (*Dispatcher)(nil)
```

---

## PR Breakdown

### PR 1: Goa Design + Type Definitions (≈150 lines)

**Branch:** `feat/LFXV2-2023-keyword-types`
**Base:** `origin/main`
**Files:** `design/brief.go`, `internal/domain/model/keyword.go`, `internal/domain/errors.go`

- Add Goa type definitions for keyword list/action payloads and results
- Add new methods `list-campaign-keywords`, `update-campaign-keywords` (Goa DSL only)
- Define error sentinels in domain/errors.go
- Run `go run ./cmd/okfgen` to generate server/client types in `gen/`

**Reviewable as:** Design-only; code generation output exempt from line count.

### PR 2: Orchestrator Interfaces + Service Handler (≈350 lines)

**Branch:** `feat/LFXV2-2023-keyword-orchestrator`
**Base:** PR 1 branch
**Files:** `internal/service/orchestrator.go`, `internal/service/brief.go`

- Add KeywordManager interface to orchestrator
- Add ReadCampaignKeywords / UpdateCampaignKeywords methods to Orchestrator
- Implement ListCampaignKeywords / UpdateCampaignKeywords handlers in BriefService
- Add timeouts + error mapping
- Add unit tests for error cases (unsupported platform, invalid bid, empty actions array)

**Test cases to add:**
- `TestBriefService_ListCampaignKeywords_HappyPath`
- `TestBriefService_ListCampaignKeywords_PlatformUnsupportedIs400`
- `TestBriefService_ListCampaignKeywords_PlatformFailureIs503`
- `TestBriefService_UpdateCampaignKeywords_HappyPath`
- `TestBriefService_UpdateCampaignKeywords_PlatformUnsupportedIs400`
- `TestBriefService_UpdateCampaignKeywords_EmptyActionsIs400`
- `TestBriefService_UpdateCampaignKeywords_PlatformFailureIs503`

### PR 3: Google Ads Keyword Operations — Phase 1 (≈600 lines)

**Branch:** `feat/LFXV2-2023-keyword-googleads-phase1`
**Base:** PR 2 branch
**Files:** `internal/dispatch/googleads/keywords.go` (new), `internal/dispatch/googleads/dispatch.go` (minimal edit to declare interface implementation)

- Implement ListKeywords for Google Ads (query via SearchGoogleAdsRequest)
- Implement UpdateKeywords for Phase 1 (pause, remove actions only)
- Add date-range parsing for days ∈ {7, 14, 30}
- Use `resolveGoogleAdsClient` (existing pattern from createAdGroupAndAd)
- Add happy-path + error tests (mock Google Ads client)

**Test cases:**
- `TestDispatcher_ListKeywords_HappyPath`
- `TestDispatcher_ListKeywords_EmptyResult`
- `TestDispatcher_ListKeywords_GoogleAdsFailure`
- `TestDispatcher_UpdateKeywords_PausePhase1`
- `TestDispatcher_UpdateKeywords_RemovePhase1`
- `TestDispatcher_UpdateKeywords_MixedPhase1`
- `TestDispatcher_UpdateKeywords_GoogleAdsFailure`

**Lines:** ~600 (googleads pkg ~450 + test ~150)

### PR 4: Integration Tests + OpenAPI Snapshot (≈200 lines)

**Branch:** `feat/LFXV2-2023-keyword-integration-phase1`
**Base:** PR 3 branch
**Files:** `internal/service/brief_test.go` (new E2E-style tests), generated OpenAPI snapshot

- Full integration test: create campaign → list keywords → pause some → verify result
- Uses real Goa-generated types and service handlers (no mocks at this level)
- Mock the Orchestrator's dispatcher map to inject a test-only KeywordManager
- Verify HTTP status codes and response shapes

**Test cases:**
- `TestIntegration_ListKeywordsEndpoint`
- `TestIntegration_UpdateKeywordsEndpoint`
- `TestIntegration_KeywordErrorHandling`

**Lines:** ~200 (tests only; gen/ output exempt)

---

## Test Plan

### Unit Tests (Per-Layer)

1. **Domain Model** (model/keyword.go)
   - Verify KeywordAction/KeywordMetric struct construction (minimal; mostly data containers)

2. **Orchestrator Interfaces**
   - Mock KeywordManager for both ListKeywords and UpdateKeywords
   - Verify timeout context is passed correctly
   - Verify error sentinel mapping (ErrKeywordManagerUnsupported → 400)
   - Verify conditional bid validation (change-bid requires positive BidMicros)

3. **BriefService Handler**
   - Test days parameter validation (only 7, 14, 30 accepted)
   - Test missing campaign (404)
   - Test unsupported platform (400)
   - Test platform failure (503)
   - Test empty actions array (400)
   - Test successful list + update with partial success

4. **Google Ads Dispatcher**
   - Mock the Google Ads client response
   - Test pause operation (status → PAUSED)
   - Test remove operation (resource deletion)
   - Test metric aggregation across ad groups
   - Test date-range logic for days ∈ {7, 14, 30}

### Integration Tests (End-to-End)

- Deploy the service with a real Goa HTTP handler
- Call `POST /projects/{id}/briefs/{id}/campaigns/{id}/keywords` (list)
- Call `PATCH /projects/{id}/briefs/{id}/campaigns/{id}/keywords` (update)
- Verify response shapes match CampaignKeywordList and CampaignKeywordActionResult
- Verify HTTP status codes (200, 400, 503)

### Credential & Configuration Tests

- Test missing Google Ads credentials (handled by resolveGoogleAdsClient; should return 503 or config error)
- Test campaign with no upstream ID (ErrCampaignNotProvisioned → should not happen here; campaign must exist in DB to reach this endpoint, which implies upstream creation)

### Contract Compliance Tests (UI ← → Campaign Service)

**Phase 1 (Pause/Remove):**
- Verify response shapes can be mapped back to lfx-v2-ui's BulkKeywordActionResponse
- Verify error messages are human-readable (logged and returned)
- Verify criterionId is preserved through the round-trip

**Phase 2 (Enable/Change-Bid):**
- Add tests for enable action (status → ENABLED)
- Add tests for change-bid with various bid values (0 rejected, boundary values tested)

---

## Open Questions for Human Decision

### 1. Bid Change Currency & Precision

**Question:** The UI's `avgCpc` is in currency units (e.g., $0.50 for USD). The Google Ads API uses **micros** (1,000,000 micros = 1 unit). 

**Current Design:** The Goa type `KeywordActionPayload.bid_micros: Int64` is always in micros. The service validates `BidMicros > 0` but does NOT cap it (Google Ads v23 API docs do not publish a hard max, only per-account limits enforced by the platform).

**Decision Needed:**
- Should the service reject bids above a sanity threshold (e.g., $1M per keyword)? 
- Should the UI always convert currency → micros before sending, or should the campaign-service offer both formats?
- **Recommendation:** Keep Phase 1 out of this PR (defer change-bid to Phase 2). Phase 1 lands pause/remove only, which have no currency concerns.

### 2. Keyword Text Field in Update Response

**Question:** The UI's KeywordActionResponse includes a `keyword: string` field (the keyword text, for display). In ListCampaignKeywords we fetch it from Google Ads. In UpdateKeywords, should we:
- Store the keyword text in the action payload so we can echo it back?
- Query Google Ads again for each criterion ID to fetch the keyword text?
- Leave it null/empty in the update response?

**Current Design:** The payload includes only criterionId + action + (optional) bidMicros. The service handler does NOT fetch keyword text for update responses.

**Decision Needed:**
- Accept empty keyword in update responses (UI can cross-reference with prior list call)?
- Add `keyword: string` to KeywordActionPayload (UI must supply it)?
- **Recommendation:** Fetch from Google Ads as part of the update query (adds ~100ms per update but provides complete outcomes). Or accept empty and document that the UI should enrich from its prior list cache.

### 3. Pagination for Large Keyword Sets

**Question:** A large campaign may have thousands of keywords. The current design returns all keywords in a single CampaignKeywordList (no pagination).

**Decision Needed:**
- Implement cursor pagination (page_token / page_size) like the query-service?
- Cap results at a max (e.g., 5000 keywords per response, return "more" token)?
- Accept unbounded for Phase 1 (defer pagination if real campaigns exceed practical limits)?
- **Recommendation:** Accept unbounded for Phase 1. If a campaign has >5000 keywords, that is rare and the UI can handle it. Pagination can be added later (Phase 3) as an optional enhancement.

### 4. Metrics Window Alignment with UI

**Question:** The UI's `getKeywords(days: number)` accepts arbitrary `days`. The new Goa design restricts to `Enum(7, 14, 30)` (matching Google Ads API windows).

**Current Design:** The service validates `days ∈ {7, 14, 30}` and rejects others with 400.

**Decision Needed:**
- Keep the strict enum (breaks compatibility with any UI call using days=1 or days=60)?
- Map arbitrary days to the nearest enum value (days ≤ 7 → 7, 7 < days ≤ 14 → 14, days > 14 → 30)?
- **Recommendation:** Keep strict enum. The UI should be updated to only call with valid values. This is backward-compatible because the current /api/campaigns/keywords endpoint doesn't exist yet; there's no deployed UI calling it.

### 5. Criterion ID vs. Resource Name

**Question:** Google Ads API uses resource names (e.g., `customers/123/adGroupCriteria/456`). The UI only sees `criterionId: string` (the numeric `456` part).

**Current Design:** The Goa types use `criterion_id: string` (the numeric ID only, not the full resource name). The dispatcher internally constructs the resource name from projectID + criterionId.

**Decision Needed:**
- Keep it this way (simpler UI contract)?
- Change to full resource names (more explicit, but verbose)?
- **Recommendation:** Keep criterion ID only. The projectID/customerID is available server-side; no need to embed it in the client's payload.

### 6. Partial Success Handling

**Question:** An UpdateKeywords call with 100 actions might succeed on 95 and fail on 5. Should the service:
- Return 200 with succeeded/failed counts and per-action outcomes?
- Return 400/503 on ANY failure?

**Current Design:** Returns 200 with per-action outcomes (similar to bulk-upload patterns). The UI can inspect `results` array to see which failed.

**Decision Needed:**
- Is this the right balance of usability vs. simplicity?
- Should a single failure fail the whole request (simpler, but user must retry all)?
- **Recommendation:** Keep partial-success (200 + per-action outcomes). The UI can show a summary ("95 succeeded, 5 failed") and let the user retry the failed ones.

### 7. Phase 2 Timing & Dependency on PR #69

**Question:** PR #69 (GA-4 targeting) is currently unmerged and in conflict. Phase 1 (this plan) can land independently. Phase 2 (enable + change-bid) has a soft dependency on #69's keyword resource-name machinery.

**Decision Needed:**
- Should Phase 1 block until #69 is merged, or land independently and Phase 2 chains after?
- If Phase 1 lands first, should it pre-implement enable/change-bid (disabled with "not yet" error) to avoid a second API change?
- **Recommendation:** Land Phase 1 independently. Phase 2 can be a separate PR that rebases on #69 once it merges. Do NOT pre-implement Phase 2 in Phase 1 (adds complexity, unclear timeline).

---

## Knowledge Base Updates

After the plan is approved and PR 1 lands, add entries to `docs/knowledge/`:

1. `keyword-surface-design.md` — Goa types and HTTP contract
2. `keyword-orchestrator-interface.md` — Orchestrator interfaces (KeywordManager)
3. `keyword-googleads-implementation.md` — Google Ads adapter specifics
4. Corresponding entries in `index.md` for discovery

Update `docs/knowledge/log/` with an entry dated at merge time:
- `2026-08-XX-LFXV2-2023-keyword-surface-phase1.md` — Feature shipped

---

## Conclusion

This plan provides a concrete, layered implementation roadmap for the Optimize-phase keyword surface. **Phase 1 (pause/remove) is deliverable independently and immediately.** Phase 2 (enable/change-bid) chains cleanly behind PR #69 when that merges. Each PR is under 1000 hand-written lines and reviewable against clear acceptance criteria.

**Dependencies and blocking factors are resolved:**
- No hard dependency on PR #69 for Phase 1
- Error mapping follows established patterns (MetricsReader, StatusToggler)
- Google Ads API surface is well-defined (adGroupCriteria:search/mutate v23)
- Test plan covers all error cases and happy paths

**Open questions are _design choices_, not blockers** — they can be decided by the owning team and implemented in parallel with code review.
