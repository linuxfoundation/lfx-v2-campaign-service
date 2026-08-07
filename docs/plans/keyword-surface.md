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

All contract shapes are from `packages/shared/src/interfaces/campaign.interface.ts:560–667` in the
`linuxfoundation/lfx-self-serve` repository (the LFX One UI).

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
- **What lands:** `GET /campaigns/{campaignId}/keywords` (list) and `POST /campaigns/{campaignId}/keyword-actions` (pause/remove only — the path already catalogued on `main`)
- **Blocked on:** Nothing in #69. Uses existing Google Ads API surfaces (adGroupCriteria:search, mutate)
- **Google Ads API:** a GAQL search over `keyword_view` for the read, and `adGroupCriteria:mutate`
  with status updates / removes for the write. Both go through `internal/platform/googleads`'s
  existing REST transport — this repo has no Google Ads SDK, so PR 3 adds the semantic client
  methods rather than calling a generated `SearchGoogleAds`/`MutateGoogleAds` surface that does
  not exist.
- **Can build on:** origin/main as-is. No keyword creation plumbing needed

### Phase 2 (With #69): Enable/Resume + Bid Changes
- **What lands:** `'enable'` action (resume paused keywords) and `'change-bid'` operation (set max CPC in micros)
- **Dependency:** #69's keyword resource-name construction and criterion-ID plumbing in `internal/platform/googleads/targeting.go`. **This file does not exist on `main` today** — PR #69 introduces it (along with `targeting_test.go`). Every reference to it in this plan is forward-looking and unresolvable until #69 merges; do not treat it as an existing API surface when scoping Phase 1.
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

The HTTP path is **not a new invention** — `POST
/projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions` is already catalogued on
`main` (`docs/api-catalog.md`, *Optimization* section). This method implements that documented row;
it must not introduce a second, differently-shaped keyword path. `keyword-actions` (an action
collection) is deliberately a POST rather than a PATCH on `.../keywords`: the request is a list of
operations to apply, not a replacement representation of the keyword set.

```goa
Method("update-campaign-keywords", func() {
  Description("Pause, enable, or change bid on keywords in a campaign. Phase 1 supports pause and remove; enable and change-bid follow in Phase 2 (LFXV2-2023). Returns 400 when the campaign's platform has no keyword-manager capability wired.")
  
  Payload(func() {
    bearerToken()
    projectIDAttr()
    briefIDAttr()
    campaignIDAttr()
    
    // MinLength(1) is not decoration. Required("actions") only checks PRESENCE, so
    // `{"actions": []}` passes generated validation and reaches a handler that has to
    // reject it by hand — the design-contract-looser-than-runtime shape called out in
    // docs/reviews/knowledge-base/api-contract-and-docs-currency.md. Put the constraint
    // in the contract so the generated client and the OpenAPI document carry it too.
    Attribute("actions", ArrayOf(KeywordActionPayload), "Keyword actions to apply", func() {
      MinLength(1)
    })
    
    Required("project_id", "brief_id", "campaign_id", "actions")
  })
  
  Result(CampaignKeywordActionResult)
  
  commonBriefErrors(true)  // Body validation
  
  HTTP(func() {
    POST("/projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keyword-actions")
    Header("bearer_token:Authorization")
    Response(StatusOK)
    briefErrorResponses(true)
  })
})
```

### Type Definitions

All types added to `design/brief.go`:

```goa
// Keyword action types. The enum carries ONLY what Phase 1 implements. Publishing
// "enable" and "change-bid" here would put them in the generated client and the OpenAPI
// document while the handler rejects them — an advertised capability that does not
// exist. Phase 2 widens this enum in the same commit that implements the two actions.
var KeywordActionEnum = func() {
  Enum("pause", "remove")
}

// A single keyword action (action item in the request).
//
// A keyword criterion is identified upstream by the COMPOSITE resource name
// `customers/{customer}/adGroupCriteria/{ad_group_id}~{criterion_id}` — see
// `internal/platform/googleads/adgroup_ad.go`, which validates exactly that shape. The
// criterion ID alone cannot address a criterion, so ad_group_id is required on every
// action, not just on the list response.
var KeywordActionPayload = Type("keyword-action-payload", func() {
  Attribute("ad_group_id", String, "Google Ads ad group ID that owns this criterion (numeric, stringified)")
  Attribute("criterion_id", String, "Google Ads criterion ID (unique within the ad group)")
  Attribute("action", String, "Action to perform", KeywordActionEnum)
  Required("ad_group_id", "criterion_id", "action")
})

// Phase 2 additions, listed here so the shape is agreed but NOT added to the design
// until the actions are implemented:
//
//   Enum("pause", "remove", "enable", "change-bid")
//   Attribute("bid_micros", Int64, "New max CPC in micros (change-bid only)")
//
// bid_micros stays optional in Goa because it applies to one action only, so the
// generated field is `*int64`. The domain model must therefore declare
// `BidMicros *int64` as well — assigning a `*int64` to an `int64` field does not
// compile, and flattening the pointer at the boundary loses the distinction between
// "omitted" and "explicitly zero", which is exactly what the change-bid validation
// needs to reject. Goa cannot express conditional Required(), so the handler validates
// (action == "change-bid") ⇒ bid_micros non-nil and positive.

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
  Attribute("ad_group_id", String, "Google Ads ad group ID that owns this criterion (numeric, stringified) — required to address the criterion on a later action")
  Attribute("ad_group_name", String, "Ad group name (for display)")
  Attribute("impressions", Int64, "Impressions in window")
  Attribute("clicks", Int64, "Clicks in window")
  Attribute("ctr", Float64, "Click-through rate (clicks/impressions; 0 when impressions=0)")
  Attribute("avg_cpc_micros", Int64, "Average CPC in micros (currency-dependent; USD for Google Ads)")
  Attribute("spend_micros", Int64, "Total spend in micros")
  Attribute("conversions", Int64, "Conversions in window")
  Attribute("max_cpc_micros", Int64, "Current max CPC bid in micros (or null if not set)")
  Attribute("google_ads_url", String, "Direct link to keyword in Google Ads UI")
  // Everything the report always returns is Required. An optional attribute generates a
  // POINTER field, so leaving avg_cpc_micros / spend_micros / conversions / google_ads_url
  // optional both fails to compile against a handler assigning plain values AND tells the
  // OpenAPI consumer that always-present data may be absent. Only genuinely nullable
  // values stay optional: quality_score (Google withholds it for new keywords) and
  // max_cpc_micros (unset when the ad group bids at group level).
  Required("criterion_id", "keyword", "match_type", "status", "ad_group_id", "ad_group_name",
    "impressions", "clicks", "ctr", "avg_cpc_micros", "spend_micros", "conversions", "google_ads_url")
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
    // Totals members are always computed, so they are values, not pointers.
    Required("impressions", "clicks", "ctr", "spend_micros", "conversions")
  })
  Required("campaign_id", "platform_campaign_id", "window", "pulled_at", "total_keywords", "keywords", "totals")
})

// Individual action result
var KeywordActionResult = Type("keyword-action-result", func() {
  Attribute("ad_group_id", String, "Ad group ID the action targeted")
  Attribute("criterion_id", String, "The keyword's criterion ID")
  Attribute("keyword", String, "Keyword text (for logging/display)")
  Attribute("action", String, "Action that was performed")
  Attribute("success", Boolean, "Whether the action succeeded")
  Attribute("message", String, "Outcome or error message")
  // message is always populated — a success reason or the failure — so it is a value,
  // not a *string the handler cannot assign to.
  Required("ad_group_id", "criterion_id", "keyword", "action", "success", "message")
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

**Generated artifacts:** After `make apigen` (`Makefile:63-68` — `goa gen` followed by the copy
of `gen/http/openapi{,3}.{json,yaml}` into `cmd/campaign-service/kodata/gen/http/`), new types
appear in `gen/lfx_v2_campaign_service_briefs/`. No manual edits here.

**Do not run `go run ./cmd/okfgen` for this.** `okfgen` regenerates the `docs/knowledge` bundle,
not Goa artifacts, and it overwrites hand-edited concept files. Running `goa gen` alone is also
wrong: it leaves the ko-embedded OpenAPI copies stale, which is exactly the drift `make apigen`
exists to prevent.

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
//
// AdGroupID is not optional context. A keyword criterion is addressed upstream by the
// COMPOSITE resource name customers/{customer}/adGroupCriteria/{ad_group_id}~{criterion_id};
// the criterion ID alone cannot name one.
type KeywordAction struct {
  AdGroupID   string // Google Ads ad group ID owning the criterion
  CriterionID string // Google Ads criterion ID
  Action      string // Phase 1: "pause" | "remove". Phase 2 adds "enable" | "change-bid"
  // BidMicros is a POINTER because the Goa attribute is optional (change-bid only) and
  // therefore generates *int64. It also has to distinguish "omitted" from an explicit 0,
  // which is what lets change-bid validation reject a missing bid rather than silently
  // treating it as zero. Phase 2 only.
  BidMicros *int64
}

// KeywordMetric holds a keyword's performance over a reporting window.
type KeywordMetric struct {
  // AdGroupID / AdGroupName are carried through from the report row: the ID because the
  // action payload needs it to build the composite resource name, the name because the UI
  // groups the table by ad group.
  AdGroupID      string
  AdGroupName    string
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
  GoogleAdsURL   string
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
  AdGroupID   string
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
// Provider and Campaign live in internal/domain/model, not internal/service — the existing
// MetricsReader contract spells them model.Provider and *model.Campaign, and so must these.
type KeywordManager interface {
  // ListKeywords fetches keywords for a campaign with their metrics over a reporting window.
  // Returns results aggregated across all ad groups in the campaign. Must never return
  // (nil, nil) — see the orchestrator's contract check below.
  ListKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign,
    days int32) (*model.KeywordListResult, error)
  
  // UpdateKeywords applies bulk actions to keywords. Returns per-action outcomes; partial
  // success is OK (failed actions don't cancel others).
  UpdateKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign,
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
//
// The two guards below are copied deliberately from ReadCampaignMetrics
// (internal/service/orchestrator.go:1015-1043), not out of symmetry: without the first, a
// campaign with no PlatformCampaignID sends a GAQL query with an empty campaign filter and
// reads the WHOLE ACCOUNT; without the second, a dispatcher returning (nil, nil) panics the
// request when the handler dereferences result.Keywords.
func (o *Orchestrator) ReadCampaignKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, days int32) (*model.KeywordListResult, error) {
  if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
    return nil, ErrCampaignNotProvisioned
  }
  d, ok := o.dispatchers[platform]
  if !ok {
    return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrKeywordManagerUnsupported, platform)
  }
  reader, ok := d.(KeywordManager)
  if !ok {
    return nil, fmt.Errorf("%w: %s", ErrKeywordManagerUnsupported, platform)
  }
  callCtx, cancel := context.WithTimeout(ctx, keywordCallTimeout) // = 30s
  defer cancel()
  res, rerr := reader.ListKeywords(callCtx, projectID, platform, campaign, days)
  if rerr != nil {
    return nil, rerr
  }
  if res == nil {
    return nil, fmt.Errorf("%s keyword manager returned a nil result with no error", platform)
  }
  return res, nil
}

// UpdateCampaignKeywords applies bulk actions to keywords in a campaign.
func (o *Orchestrator) UpdateCampaignKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, actions []*model.KeywordAction) ([]*model.KeywordActionOutcome, error) {
  if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
    return nil, ErrCampaignNotProvisioned
  }
  // Phase 2 only. The error WRAPS ErrInvalidBidChange — the handler classifies with
  // errors.Is, so a bare fmt.Errorf would fall through to the default branch and return
  // 503 for what is plainly a 400.
  for _, act := range actions {
    if act.Action == "change-bid" && (act.BidMicros == nil || *act.BidMicros <= 0) {
      return nil, fmt.Errorf("%w: change-bid requires a positive bid_micros", ErrInvalidBidChange)
    }
  }
  
  d, ok := o.dispatchers[platform]
  if !ok {
    return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrKeywordManagerUnsupported, platform)
  }
  manager, ok := d.(KeywordManager)
  if !ok {
    return nil, fmt.Errorf("%w: %s", ErrKeywordManagerUnsupported, platform)
  }
  callCtx, cancel := context.WithTimeout(ctx, keywordCallTimeout) // = 30s
  defer cancel()
  outcomes, uerr := manager.UpdateKeywords(callCtx, projectID, platform, campaign, actions)
  if uerr != nil {
    return nil, uerr
  }
  if outcomes == nil {
    return nil, fmt.Errorf("%s keyword manager returned a nil result with no error", platform)
  }
  return outcomes, nil
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
    case errors.Is(merr, ErrCampaignNotProvisioned):
      // Same mapping GetCampaignMetrics uses: the campaign exists locally but has nothing
      // upstream yet, so there is nothing to read. 409, not 503 — retrying will not help
      // until the campaign is dispatched.
      return nil, &briefs.ConflictError{Code: "409", Message: "campaign has not been provisioned on the ad platform yet"}
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
      AdGroupID:    kw.AdGroupID,
      AdGroupName:  kw.AdGroupName,
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
      GoogleAdsURL: kw.GoogleAdsURL,
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
  
  // Defence in depth. With MinLength(1) on the Goa attribute the generated validator
  // already rejects an empty array, so this branch should be unreachable through HTTP —
  // it guards the direct-call path and documents the invariant.
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
      AdGroupID:   act.AdGroupID,
      CriterionID: act.CriterionID,
      Action:      act.Action,
      // Phase 2. BidMicros is *int64 on BOTH sides, so this is a straight pointer copy —
      // no dereference, which would panic for every pause/remove action.
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
    case errors.Is(aerr, ErrCampaignNotProvisioned):
      return nil, &briefs.ConflictError{Code: "409", Message: "campaign has not been provisioned on the ad platform yet"}
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
      AdGroupID:   outcome.AdGroupID,
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

**File to edit:** `internal/dispatch/googleads.go`

`internal/dispatch` is a **flat package — one file per platform**, with each optional capability
added as a method on that platform's dispatcher type. There is no `internal/dispatch/googleads/`
subpackage and no `dispatch.go`; the type is `GoogleAdsDispatcher` (`googleads.go:43`), and
`ToggleStatus` (`:252`) is the precedent for how a capability is attached. Keyword methods go
alongside it in the same file. If `googleads.go` grows unwieldy, the split to make is a sibling
flat file (`googleads_keywords.go`), not a new package — a package would break the shared
unexported helpers (`googleAdsConfig`, `validateGoogleAdsConnection`, `resolveGoogleAdsClient`).

#### The platform-client boundary comes first

There is no Google Ads SDK in this repo. `internal/platform/googleads` is a hand-rolled REST
client, and its transport (`doRequest`), its GAQL helper (`gaqlSearch`, `client.go:771`), and its
customer/account fields are all **unexported**. `resolveGoogleAdsClient` returns only a
`*googleads.Client` — there is no `creds` value in scope in the dispatcher, and no
`SearchGoogleAds` / `MutateGoogleAds` method to call.

So the keyword work is **two layers, not one**, and PR 3 must contain both:

1. `internal/platform/googleads/keywords.go` — two new *semantic* methods on `*Client`:
   `ListCampaignKeywords(ctx, campaignID string, startDate, endDate string) ([]KeywordRow, error)`
   and `MutateKeywordCriteria(ctx, ops []KeywordCriterionOp) ([]KeywordCriterionResult, error)`.
   These own the GAQL text, the resource-name construction, the response decoding, and the
   malformed-response classification — exactly where `CreateCampaign` and `UpdateCampaignStatus`
   already put that work.
2. `internal/dispatch/googleads.go` — `ListKeywords` / `UpdateKeywords` translate between
   `model.*` and those client types. No HTTP, no GAQL, no resource names.

**Implementation outline** (Phase 1: pause/remove only):

```go
// ---- internal/platform/googleads/keywords.go -------------------------------------

// The GAQL query. Field paths are snake_case: GAQL is NOT the protobuf JSON
// representation, and v23 rejects camelCase paths outright — the query fails before a
// single row comes back. There is also no `metrics.ctr_percent` field; `metrics.ctr` is
// already a fraction, and we recompute totals CTR ourselves regardless.
//
// The campaign filter is what scopes the read. It is applied here, in the client, so no
// caller can accidentally issue an account-wide keyword query.
const keywordReportQuery = `
  SELECT ad_group_criterion.criterion_id,
         ad_group_criterion.keyword.text,
         ad_group_criterion.keyword.match_type,
         ad_group_criterion.status,
         ad_group_criterion.quality_info.quality_score,
         ad_group_criterion.effective_cpc_bid_micros,
         ad_group.id,
         ad_group.name,
         campaign.id,
         metrics.impressions,
         metrics.clicks,
         metrics.cost_micros,
         metrics.conversions,
         metrics.average_cpc
  FROM keyword_view
  WHERE campaign.id = %s
    AND segments.date BETWEEN '%s' AND '%s'
  ORDER BY metrics.impressions DESC`
```

`campaignID` and the two dates must be validated before interpolation — the same GAQL-injection
guard `internal/platform/googleads/metrics.go` already applies to its window literal. `campaignID`
is digits-only; the dates are produced internally by `dateRangeForDays` and are never
caller-supplied.

```go
// ---- criterion authorization ------------------------------------------------------

// AuthorizeKeywordCriteria returns the subset of (adGroupID, criterionID) pairs that
// actually belong to campaignID, so the caller can reject the rest.
//
// This is not belt-and-braces. The route is campaign-scoped, but the criterion IDs come
// from the request body: without this, anyone who may manage ONE campaign in a project
// can pause or remove keywords in ANY campaign under the same Google Ads customer, simply
// by supplying that campaign's criterion IDs. Resolve every requested pair against the
// campaign before building a single mutate operation.
func (c *Client) AuthorizeKeywordCriteria(ctx context.Context, campaignID string,
  want []KeywordCriterionRef) (map[KeywordCriterionRef]string, error)
```

```go
// ---- resource names ---------------------------------------------------------------

// An ad-group criterion resource name is COMPOSITE:
//   customers/{customer}/adGroupCriteria/{ad_group_id}~{criterion_id}
// The customer ID plus criterion ID is not sufficient, and
// internal/platform/googleads/adgroup_ad.go already validates exactly this four-segment,
// tilde-joined shape. This is why ad_group_id is carried on every KeywordAction rather
// than only appearing in the list response.
func (c *Client) keywordCriterionResourceName(adGroupID, criterionID string) string {
  return c.customerPath(fmt.Sprintf("adGroupCriteria/%s~%s", adGroupID, criterionID))
}
```

```go
// ---- internal/dispatch/googleads.go ------------------------------------------------

// ListKeywords returns keywords for a campaign with their metrics over a reporting window.
func (d *GoogleAdsDispatcher) ListKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, days int32) (*model.KeywordListResult, error) {

  client, err := d.resolveGoogleAdsClient(ctx, projectID, platform)
  if err != nil {
    return nil, fmt.Errorf("could not resolve google ads client: %w", err)
  }

  startDate, endDate := dateRangeForDays(days)
  rows, err := client.ListCampaignKeywords(ctx, campaign.PlatformCampaignID, startDate, endDate)
  if err != nil {
    return nil, err
  }

  // make(..., 0, n), never `var keywords []*model.KeywordMetric`: a campaign with zero
  // keywords is an empty list, not a failure, and the orchestrator's nil-result contract
  // check would otherwise turn a correct empty answer into a 503. Same trap as
  // GoogleAdsDispatcher.ListAccounts.
  keywords := make([]*model.KeywordMetric, 0, len(rows))
  result := &model.KeywordListResult{}

  for _, row := range rows {
    keywords = append(keywords, &model.KeywordMetric{
      AdGroupID:    row.AdGroupID,
      AdGroupName:  row.AdGroupName,
      CriterionID:  row.CriterionID,
      Keyword:      row.Text,
      MatchType:    row.MatchType,       // EXACT | PHRASE | BROAD
      Status:       strings.ToLower(row.Status),
      QualityScore: row.QualityScore,    // nil when Google withholds it
      Impressions:  row.Impressions,
      Clicks:       row.Clicks,
      Ctr:          ratio(row.Clicks, row.Impressions),
      AvgCpcMicros: row.AverageCpcMicros,
      SpendMicros:  row.CostMicros,
      Conversions:  row.Conversions,
      MaxCpcMicros: row.EffectiveCpcBidMicros,
      GoogleAdsURL: keywordDeepLink(client.CustomerID(), row.CampaignID, row.AdGroupID, row.CriterionID),
    })
    result.Impressions += row.Impressions
    result.Clicks += row.Clicks
    result.SpendMicros += row.CostMicros
    result.Conversions += row.Conversions
  }

  result.Keywords = keywords
  result.Ctr = ratio(result.Clicks, result.Impressions)
  return result, nil
}

// ratio guards the denominator. A non-empty keyword list can still total ZERO impressions
// — every keyword newly added, or the window predating the campaign — and `x/0` in Go
// float arithmetic yields +Inf or NaN, neither of which encodes as a JSON number. The
// contract says CTR is 0 when impressions are 0; this is what makes that true. Guarding on
// `len(keywords) > 0` does not: the list being non-empty says nothing about impressions.
func ratio(num, den int64) float64 {
  if den == 0 {
    return 0
  }
  return float64(num) / float64(den)
}

// UpdateKeywords applies bulk actions to keywords. Phase 1: pause, remove.
func (d *GoogleAdsDispatcher) UpdateKeywords(ctx context.Context, projectID string, platform model.Provider,
  campaign *model.Campaign, actions []*model.KeywordAction) ([]*model.KeywordActionOutcome, error) {

  client, err := d.resolveGoogleAdsClient(ctx, projectID, platform)
  if err != nil {
    return nil, fmt.Errorf("could not resolve google ads client: %w", err)
  }

  // Authorize BEFORE building any operation. Anything not resolvable inside this campaign
  // is refused as a per-action failure — never silently applied, and never a whole-request
  // error, since partial success is the documented contract.
  want := make([]googleads.KeywordCriterionRef, 0, len(actions))
  for _, a := range actions {
    want = append(want, googleads.KeywordCriterionRef{AdGroupID: a.AdGroupID, CriterionID: a.CriterionID})
  }
  allowed, aerr := client.AuthorizeKeywordCriteria(ctx, campaign.PlatformCampaignID, want)
  if aerr != nil {
    return nil, aerr
  }

  outcomes := make([]*model.KeywordActionOutcome, 0, len(actions))
  for _, action := range actions {
    ref := googleads.KeywordCriterionRef{AdGroupID: action.AdGroupID, CriterionID: action.CriterionID}
    outcome := &model.KeywordActionOutcome{
      AdGroupID:   action.AdGroupID,
      CriterionID: action.CriterionID,
      Action:      action.Action,
      Keyword:     allowed[ref], // the keyword text, resolved during authorization
    }

    if _, ok := allowed[ref]; !ok {
      outcome.Success = false
      outcome.Message = "criterion does not belong to this campaign"
      outcomes = append(outcomes, outcome)
      continue
    }

    // Phase 1 vocabulary only. The Goa enum already rejects anything else at the edge;
    // this branch covers the direct-call path and is where Phase 2 adds "enable" and
    // "change-bid".
    var op googleads.KeywordCriterionOp
    switch action.Action {
    case "pause":
      op = googleads.PauseKeyword(ref)
    case "remove":
      op = googleads.RemoveKeyword(ref)
    default:
      outcome.Success = false
      outcome.Message = fmt.Sprintf("unsupported action: %s", action.Action)
      outcomes = append(outcomes, outcome)
      continue
    }

    // One operation per call keeps per-action attribution exact: a batched mutate fails
    // atomically, so a single bad criterion would report every other action as failed.
    res, merr := client.MutateKeywordCriteria(ctx, []googleads.KeywordCriterionOp{op})
    switch {
    case merr != nil:
      outcome.Success = false
      // The platform client already classifies and redacts upstream errors; do not
      // re-render the raw response body into a user-visible message.
      outcome.Message = merr.Error()
    case len(res) == 0:
      // A 2xx with no results is a malformed response, not a success. Indexing res[0]
      // here — as an earlier draft of this plan did, and again into Errors[0] — panics
      // the request instead of producing a controlled platform error. Length checks
      // belong in the client, which is why MutateKeywordCriteria returns a typed slice.
      outcome.Success = false
      outcome.Message = "google ads returned no result for this operation"
    default:
      outcome.Success = res[0].OK
      outcome.Message = res[0].Message
    }

    outcomes = append(outcomes, outcome)
  }

  return outcomes, nil
}

// dateRangeForDays returns an INCLUSIVE start/end pair for a reporting window.
//
// GAQL's `segments.date BETWEEN a AND b` includes both endpoints, so a 30-day window is
// `now-29 .. now`, not `now-30 .. now` — the latter is 31 calendar dates and would
// disagree with Google's own LAST_30_DAYS and with this service's MetricsWindow mapping.
// The clock is read ONCE: two separate time.Now() calls can straddle midnight UTC and
// produce a window one day wider than requested.
func dateRangeForDays(days int32) (string, string) {
  now := time.Now().UTC()
  end := now.Format("2006-01-02")
  start := now.AddDate(0, 0, -(int(days) - 1)).Format("2006-01-02")
  return start, end
}
```

**Note on `LAST_30_DAYS` semantics.** Google's own `LAST_30_DAYS` excludes today. The window above
*includes* today, because the UI's `days` parameter has always meant "the last N days including
the partial current day" and the keyword table is used for intra-day decisions. That is a
deliberate divergence, not an oversight — Open Question 4 asks whether to keep it.

**File to edit:** `internal/dispatch/googleads.go` (same file — the capability assertion sits with
the methods it asserts about)

**Declare that `GoogleAdsDispatcher` satisfies the KeywordManager capability:**

```go
// Package-level assertion, alongside the ListKeywords/UpdateKeywords methods:
var _ service.KeywordManager = (*GoogleAdsDispatcher)(nil)
```

The interface is declared in `internal/service` (the orchestrator's package), matching where
`MetricsReader` lives — the dispatch package imports the service package's capability interfaces,
not an `orchestrator` package (no such package exists).

---

## PR Breakdown

> **On the `Base:` fields below.** These are written as a stack (each PR based on the previous),
> which conflicts with the repo's standing convention that **every PR targets `main`**. The Google
> Ads stack (#66→#67→#68→#69→#70) is the cautionary case: it is currently blocked because #69 has
> an unresolvable conflict against its base rather than against `main`, and each unmerged parent
> multiplies the rebase cost of everything above it.
>
> Prefer landing PR 1 into `main` first and rebasing PR 2 onto `main` once PR 1 merges, rather than
> opening all four as a stack up front. If review latency makes serialization impractical, note
> that the `Base:` values below are a **build order**, not an instruction to open four chained PRs
> simultaneously.

### PR 1: Goa Design + Type Definitions (≈150 lines)

**Branch:** `feat/LFXV2-2023-keyword-types`
**Base:** `origin/main`
**Files:** `design/brief.go`, `internal/domain/model/keyword.go`, `internal/domain/errors.go`

- Add Goa type definitions for keyword list/action payloads and results
- Add new methods `list-campaign-keywords`, `update-campaign-keywords` (Goa DSL only)
- Define error sentinels in domain/errors.go
- Run `make apigen` to regenerate `gen/` **and** the ko-embedded OpenAPI copies under
  `cmd/campaign-service/kodata/gen/http/`. Not `goa gen` alone (leaves kodata stale) and
  definitely not `go run ./cmd/okfgen`, which regenerates the knowledge bundle.

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
**Files:** `internal/platform/googleads/keywords.go` **(new — this PR must include the platform
client, not just the adapter)**, `internal/platform/googleads/keywords_test.go`,
`internal/dispatch/googleads.go` (add `ListKeywords`/`UpdateKeywords` plus the `KeywordManager`
assertion), `internal/dispatch/googleads_test.go`

- Platform client: `ListCampaignKeywords`, `AuthorizeKeywordCriteria`, `MutateKeywordCriteria`,
  composite resource-name construction, GAQL text (snake_case, campaign-scoped), response
  length validation
- Dispatcher: model translation, totals accumulation with a guarded CTR, per-action outcomes
- Add date-range parsing for days ∈ {7, 14, 30} (inclusive, single clock read)
- Use `d.resolveGoogleAdsClient` (`internal/dispatch/googleads.go:202`); `ToggleStatus` (`:252`) is the closest existing caller to model on

**Test cases:**
- `TestClient_ListCampaignKeywords_QueryIsSnakeCaseAndCampaignScoped` (pins the GAQL text —
  camelCase paths are rejected upstream and no unit test would otherwise notice)
- `TestClient_KeywordCriterionResourceName_IsComposite`
- `TestClient_MutateKeywordCriteria_EmptyResultsIsErrorNotPanic`
- `TestDispatcher_ListKeywords_HappyPath`
- `TestDispatcher_ListKeywords_EmptyResultIsEmptySliceNotNil`
- `TestDispatcher_ListKeywords_ZeroImpressionsCtrIsZeroNotNaN`
- `TestDispatcher_ListKeywords_GoogleAdsFailure`
- `TestDispatcher_UpdateKeywords_PausePhase1`
- `TestDispatcher_UpdateKeywords_RemovePhase1`
- `TestDispatcher_UpdateKeywords_ForeignCriterionIsRejected` (criterion from another campaign
  in the same customer must fail, not mutate)
- `TestDispatcher_UpdateKeywords_MixedPhase1`
- `TestDispatcher_UpdateKeywords_GoogleAdsFailure`
- `TestDateRangeForDays_IsInclusiveAndExactlyNDays`

**Lines:** ~750 (platform client ~300 + dispatcher ~200 + tests ~250). Over the ~600 originally
budgeted because the platform-client layer was missing from that estimate; still under the
1000-line cap, but if it grows, split the platform client into its own PR ahead of the adapter.

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
- Call `GET /projects/{id}/briefs/{id}/campaigns/{id}/keywords` (list) — the method and path
  the design declares. An earlier draft wrote POST here and PATCH for the update, neither of
  which is routed; those tests would have verified a 405/404 and passed for the wrong reason.
- Call `POST /projects/{id}/briefs/{id}/campaigns/{id}/keyword-actions` (update)
- Verify response shapes match CampaignKeywordList and CampaignKeywordActionResult
- Verify HTTP status codes (200, 400, 409, 503)
- Verify `{"actions": []}` is rejected by the GENERATED validator (400), not only by the handler

### Credential & Configuration Tests

- Test missing Google Ads credentials (handled by resolveGoogleAdsClient; should return 503 or config error)
- Test campaign with no upstream ID → 409. A campaign row exists from the moment the brief is
  dispatched, and `platform_campaign_id` stays empty until the platform call succeeds — so a
  locally-present, upstream-absent campaign is an ordinary state, not an impossible one. Without
  the guard the GAQL query carries an empty campaign filter and reads the whole account.

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

**Resolved by the authorization step.** `AuthorizeKeywordCriteria` already has to query the
campaign's criteria to prove each requested pair belongs to it, so it returns the keyword text
for free. The outcome carries it with no extra round-trip and no `keyword` field on the request
payload. This question only existed while authorization was missing from the design.

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

**Resolved — the original framing was wrong.** An ad-group criterion resource name is
`customers/{customer}/adGroupCriteria/{ad_group_id}~{criterion_id}`, a COMPOSITE of the ad group
and the criterion, as `internal/platform/googleads/adgroup_ad.go` already validates. The customer
ID is available server-side, but the ad group ID is not derivable from the criterion ID — so
"criterion ID only" cannot address a criterion at all.

The contract therefore carries `ad_group_id` alongside `criterion_id` on every action. The UI
already has it: `adGroupId` is in the list response it renders from. The full resource name stays
server-side, constructed in the platform client, so the customer ID is never in the payload.

The remaining choice — carry the pair, or carry a validated full resource name — is settled in
favour of the pair: a client-supplied resource name would have to be re-validated against the
customer anyway, which is the same work with a larger attack surface.

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

**This PR carries its own.** `CLAUDE.md:17-26` requires every merged PR to update the relevant
concept and add a dated log fragment; deferring that to PR 1 would put this one outside the merge
contract. The plan is itself a durable decision record, so it ships with:

- `docs/knowledge/log/2026-08-06-lfxv2-2023-keyword-surface-plan.md` — what was decided and, more
  usefully, the four contract facts the first draft got wrong (GAQL is snake_case; criterion
  resource names are composite; `make apigen` not `okfgen`; optional Goa attributes generate
  pointers).

Then, as each implementation PR lands:

1. `docs/knowledge/code/design-and-gen.md` — the two new methods and their types (PR 1)
2. `docs/knowledge/code/internal-service.md` — the `KeywordManager` optional capability (PR 2)
3. `docs/knowledge/code/internal-platform-googleads.md` — the keyword client surface, the GAQL
   query, and criterion authorization (PR 3)
4. `docs/api-catalog.md` — the two endpoint rows, with the `keyword-actions` row updated from
   planned to implemented
5. A dated `docs/knowledge/log/` fragment per PR

---

## Conclusion

This plan provides a concrete, layered implementation roadmap for the Optimize-phase keyword surface. **Phase 1 (pause/remove) is deliverable independently and immediately.** Phase 2 (enable/change-bid) chains cleanly behind PR #69 when that merges. Each PR is under 1000 hand-written lines and reviewable against clear acceptance criteria.

**Dependencies and blocking factors are resolved:**
- No hard dependency on PR #69 for Phase 1
- Error mapping follows established patterns (MetricsReader, StatusToggler)
- The Google Ads surface is reached through `internal/platform/googleads`, which PR 3 extends
  with keyword read/mutate/authorize methods — there is no SDK and no exported search or mutate
  entry point to call from the dispatcher
- Test plan covers all error cases and happy paths

**Open questions are _design choices_, not blockers** — they can be decided by the owning team and implemented in parallel with code review.
