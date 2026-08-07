<!-- Copyright The Linux Foundation and each contributor to LFX.
SPDX-License-Identifier: MIT -->

# Keyword Surface Implementation Plan (LFXV2-2023 Roadmap Item 4)

**Status:** Research phase complete. Plan ready for human decision on dependencies and phasing.

**Goal:** Expose keywords for a campaign and actions to manage them, replacing the UI's direct Google Ads API calls at:
- `GET /api/campaigns/keywords` (list keywords with metrics)
- `POST /api/campaigns/keywords/actions` (Phase 1: `pause` / `remove`; Phase 2 adds `enable` / `change-bid`)

The new campaign-service endpoints will follow the `MetricsReader` optional-capability pattern established by `get-campaign-metrics`, with keywords managed via an analogous `KeywordManager` interface.

---

## Current UI Contract (Source of Truth)

All contract shapes below are transcribed from the LFX One UI's shared interfaces, pinned to a
commit so a reviewer can diff this plan against the exact revision it was written from:

[`packages/shared/src/interfaces/campaign.interface.ts#L563-L667`](https://github.com/linuxfoundation/lfx-self-serve/blob/f1c6fab38193197339caae68458e33e6b16089fd/packages/shared/src/interfaces/campaign.interface.ts#L563-L667)
(`linuxfoundation/lfx-self-serve` @ `f1c6fab3`) — `KeywordMetrics` at L563, `KeywordTotals` at
L582, `KeywordMetricsResponse` at L590, `KeywordActionType` at L640, and the request/response
action types at L642–L667. A local checkout path is not a citation: it names no revision and no
reviewer can open it.

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
  ctr: number;                     // PERCENTAGE POINTS, not a ratio: 5.0 means 5%. The BFF
                                   // stores `metrics.ctr * 100` (campaign-metrics.service.ts:383)
                                   // and the display pipe only appends `%`. See the CTR note below.
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
  campaignId: string;              // GOOGLE ADS campaign id (GAQL `campaign.id`), NOT a
                                   // campaign-service UUID — see the identity-gap note below
  adGroupId: string;               // GOOGLE ADS ad group id (GAQL `ad_group.id`), likewise
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

### The identity gap — this endpoint cannot be a drop-in replacement

Every id the current surface carries is a **Google Ads** id, read straight out of GAQL:
`campaignId` ← `campaign.id`, `adGroupId` ← `ad_group.id`, `criterionId` ←
`ad_group_criterion.criterion_id` (`campaign-metrics.service.ts:370-380`). None of them is a
campaign-service UUID, and the view they come from is **account-wide**: `getKeywords` runs one
GAQL query over `keyword_view` across the authenticated user's whole Google Ads account, with
no project, brief, or local campaign in scope at all.

The endpoint proposed below is scoped `/{projectId}/briefs/{briefId}/campaigns/{campaignId}/…`
and takes the campaign-service UUID as its path parameter. **The current UI does not have those
three values on this screen**, so it cannot call the new endpoint by swapping a URL. That is a
real migration, not a rename, and pretending otherwise is how this plan would ship a Phase 1
nothing can consume.

Two ways to close it; the owning team picks one (tracked as Open Question 8):

1. **Scope the optimization view to a campaign.** The keyword table moves under a selected
   campaign, so the UI already holds project/brief/campaign and the new path is directly
   callable. Cleanest contract, biggest UX change — the account-wide "all my keywords" view
   goes away or becomes a separate surface.
2. **Resolve Google Ads ids back to local campaigns.** The service already persists
   `PlatformCampaignID` per (brief, platform), so a lookup from `campaign.id` to the local
   campaign row is possible. Preserves the current view, but only covers keywords under
   campaigns THIS service created — anything created directly in Ads Manager has no local row
   and cannot be actioned, which the UI would have to represent.

Until one is chosen, the read endpoint below is additive (a new campaign-scoped view) and does
**not** retire `GET /api/campaigns/keywords`. The retirement is UI work in `lfx-self-serve`,
sequenced after the choice, and out of scope for the PRs in this plan.

---

## Dependency Analysis: PR #69 vs. Plan Item 4

**Verdict: the dependency is GONE.** PR #69 (GA-4 targeting) merged to `main` on
**2026-08-07 at 15:37 UTC**, and `internal/platform/googleads/targeting.go` is present on `main`
today. The two-phase structure below was written to route around an unmerged #69; it is kept
because the phases are still a sensible *delivery* split (pause/remove is the smaller, safer
surface and can ship first), but Phase 2 is no longer BLOCKED on anything — it is a scope
choice, not a wait. Every statement below that treated #69 as pending has been corrected.

### Phase 1 (No #69 dependency): List Keywords + Pause/Remove Actions
- **What lands:** `GET /campaigns/{campaignId}/keywords` (list) and `POST /campaigns/{campaignId}/keyword-actions` (pause/remove only — the path already catalogued on `main`)
- **Blocked on:** Nothing in #69. Uses existing Google Ads API surfaces: `googleAds:search` for
  the GAQL read and `adGroupCriteria:mutate` for the write. (There is no
  `adGroupCriteria:search` endpoint — v23 has exactly one GAQL read surface, and this repo's
  client already encodes the split at `internal/platform/googleads/client.go:725-772`.)
- **Google Ads API:** a GAQL search over `keyword_view` for the read, and `adGroupCriteria:mutate`
  with status updates / removes for the write. Both go through `internal/platform/googleads`'s
  existing REST transport — this repo has no Google Ads SDK, so PR 2 adds the semantic client
  methods rather than calling a generated `SearchGoogleAds`/`MutateGoogleAds` surface that does
  not exist.
- **Can build on:** origin/main as-is. No keyword creation plumbing needed

### Phase 2 (With #69): Enable/Resume + Bid Changes
- **What lands:** `'enable'` action (resume paused keywords) and `'change-bid'` operation (set max CPC in micros)
- **Dependency:** #69's keyword resource-name construction and criterion-ID plumbing in `internal/platform/googleads/targeting.go`. That file is **on `main` now** (#69 merged 2026-08-07 15:37 UTC), so references to it in this plan resolve against an existing API surface and can be read while scoping.
- **Why:** The bid-change mutation uses the same criterion resource name machinery #69 implements for creation; reusing it is safer than duplicating it
- **Timing:** No longer gated. Branch from `origin/main` and the machinery is there.

**Implication:** Phase 1 and Phase 2 are now purely a scope decision — ship pause/remove first if you want the smaller review, or do both. Neither waits on another PR.

### Phase 2 surface (not in PR 1)

Everything `change-bid` needs is listed here in one place so the Phase 2 commit is mechanical,
and so PR 1 carries none of it. The rule the rest of this plan follows: **a phase ships the
enum widening and the code that serves it in the same commit, or ships neither.**

An earlier draft put `BidMicros`, `ErrInvalidBidChange`, the orchestrator's bid-validation
loop, and the handler's 400 arm into PR 1 as "groundwork". That is not groundwork, it is dead
surface. The Phase 1 Goa enum is `Enum("pause", "remove")`, so the generated request validation
rejects `change-bid` before any of it runs — there is no input a reviewer or a test can supply
that reaches the validation loop or the error arm. A guard no test can reach is exactly the
defect class this repo's review process flags (see the metrics-window guards on #70), and a
sentinel nothing returns reads as a shipped capability when it is not one.

The Phase 2 commit adds, together:

| Where | Addition |
|---|---|
| `design/brief.go` | `Enum("pause", "remove", "enable", "change-bid")`; `Attribute("bid_micros", Int64, …)` — optional, so the generated field is `*int64` |
| `internal/domain/errors.go` | `ErrInvalidBidChange = errors.New("bid value is outside the platform's supported range")` |
| `internal/domain/model/keyword.go` | `BidMicros *int64` on `KeywordAction` — a pointer, so "omitted" is distinguishable from an explicit `0`, which is the case the validation must reject |
| `internal/service/orchestrator.go` | the `(action == "change-bid") ⇒ bid_micros non-nil and positive` loop, wrapping `ErrInvalidBidChange` with `%w` so the handler's `errors.Is` matches (a bare `fmt.Errorf` falls through to the default arm and returns 503 for a plain 400) |
| `internal/service/brief.go` | `BidMicros: act.BidMicros` in the payload→domain conversion, and the `errors.Is(aerr, ErrInvalidBidChange)` → 400 arm |
| `internal/dispatch/googleads.go` | the `"enable"` and `"change-bid"` cases in the action switch |
| tests | enable → `ENABLED`; change-bid with a valid bid, a missing bid, `0`, and a negative bid |

`enable` is the same shape minus the bid: enum entry, one switch case, one test. It is grouped
into Phase 2 rather than Phase 1 only because both are status-adjacent follow-ups and one
design change is cheaper than two — not because it is blocked.

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
    
    // Reporting window. NO `Default(30)` here, deliberately: a Goa attribute with a
    // default is generated as a VALUE (`Days int32`), not a pointer, because Goa fills
    // the default in during decoding so the field can never be absent. The handler below
    // checks `p.Days != nil` and dereferences it, which does not compile against a
    // non-pointer field. Either the DSL owns the default or the handler does, and the
    // handler is the better owner here: it already validates the enum a second time for
    // non-generated callers, so keeping both the default and the validation in one place
    // means there is exactly one statement of what "unspecified" means.
    Attribute("days", Int32, "Reporting window in days (7, 14, 30); defaults to 30", func() {
      Enum(7, 14, 30)
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
`main` (`docs/api-catalog.md:108`, *Optimization* section). This method implements that documented
row; it must not introduce a second, differently-shaped keyword path. `keyword-actions` (an action
collection) is deliberately a POST rather than a PATCH on `.../keywords`: the request is a list of
operations to apply, not a replacement representation of the keyword set.

**This has to be reconciled with rule 5, "No bulk mutation endpoints" (`docs/api-catalog.md:19`),
before implementation — and it is not enough that the catalog already reserves the row.** The rule
gives two reasons, and both have to be answered on their own terms rather than waved past:

1. *"HTTP cannot cleanly express partial success/failure across a set."* The rule is about
   endpoints where the status code is the only outcome channel. Here it is not: the result type
   carries `succeeded`, `failed`, and a per-action `results` array with an explicit `success` and
   `message` on every entry, so the response says precisely which criteria changed and which did
   not. The 200 is a statement about the request being processed, and nothing in it is read as
   "everything worked".
2. *"A single bulk call cuts across per-target permission boundaries."* This is the load-bearing
   reason, and it is the one that genuinely does not apply — but only because of the authorization
   step added below. Every criterion in the request is resolved against **this campaign** before a
   single operation is built, and the route is gated on `campaign_manager` for **this project**.
   The permission-evaluated target is the campaign, and there is exactly one of them per request.
   Without `AuthorizeKeywordCriteria` the rule's objection would be exactly right: criterion IDs
   are caller-supplied, so an unauthorized batch really would span every campaign in the customer.

The rule's prohibited shape is the omitted "bulk status/budget change across campaigns", where N
independently-permissioned resources are mutated in one call. This endpoint has a single
permissioned target and an itemized outcome, so it is an exception with a stated rationale rather
than a violation — **but it must be written into `docs/api-catalog.md` as a named exception in the
PR that implements it.** An exception argued only in a plan document is one an implementer or a
future reviewer will read as an oversight.

```goa
Method("update-campaign-keywords", func() {
  // The description is generated verbatim into the OpenAPI document, so it must name the
  // actions this method ACCEPTS. "Pause, enable, or change bid" contradicted both the next
  // sentence and the request enum, and omitted `remove` — a core Phase 1 operation.
  Description("Pause or remove keywords in a campaign. Returns 400 when the campaign's platform has no keyword-manager capability wired. Phase 2 (LFXV2-2023) widens this to enable and change-bid, in the same commit that implements them.")
  
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
    // MaxLength as well as MinLength. The dispatcher rationale below fixes the supported
    // scale at 100 actions and gives the whole synchronous operation a 30-second budget; an
    // UNBOUNDED array lets a caller force arbitrary mutate chunks, blow that budget mid-batch,
    // and manufacture exactly the unconfirmed-outcome case the rest of this design works to
    // avoid. The cap belongs in the contract, not in a handler check, so the generated client
    // and the OpenAPI document both carry it and a caller learns the limit before sending.
    Attribute("actions", ArrayOf(KeywordActionPayload), "Keyword actions to apply", func() {
      MinLength(1)
      MaxLength(100)
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
  // A RATIO (0.05 = 5%), matching Google's own `metrics.ctr` and this service's existing
  // CampaignMetrics.Ctr. The current UI contract is NOT a ratio: its BFF stores
  // `metrics.ctr * 100` and the display pipe only appends `%`, so a keyword row consumed
  // straight from this endpoint would render "0.05%" instead of "5%". Deliberate — the new
  // endpoint stays consistent with every other metric surface this service exposes rather
  // than inheriting one screen's convention — but it is a REQUIRED UI conversion, not a
  // drop-in: the consumer multiplies by 100 at the boundary. Listed with the identity gap
  // above as part of the migration work, and out of scope for the PRs in this plan.
  Attribute("ctr", Float64, "Click-through rate as a RATIO: clicks/impressions, 0 when impressions=0 (multiply by 100 for percent)")
  // Micros are denominated in the AD ACCOUNT's own currency, not USD — the same fact
  // docs/api-catalog.md:318-321 records for Google Ads budgets. The service does no FX
  // conversion, so naming USD in the generated schema would mislead every non-USD account.
  Attribute("avg_cpc_micros", Int64, "Average CPC in micros of the ad account's currency")
  Attribute("spend_micros", Int64, "Total spend in micros of the ad account's currency")
  // Float64, not Int64: Google Ads v23 types `metrics.conversions` as a DOUBLE, and
  // attributed conversions are routinely fractional (a conversion split across several
  // touchpoints contributes a fraction to each). Int64 either rejects the fractional JSON
  // value during decoding or silently truncates it, and truncation toward zero systematically
  // UNDER-reports the metric the keyword table exists to optimise against. Float64 all the way
  // through — model, totals, and dispatcher accumulation.
  Attribute("conversions", Float64, "Conversions in window (fractional; Google attributes partial conversions)")
  Attribute("max_cpc_micros", Int64, "Criterion-level max CPC bid in micros of the ad account's currency (null when the keyword bids at ad-group level)")
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
  // Int32 with the SAME closed enum as the request's `days`, echoed back so a client that
  // relied on the default knows which window it got. A String here would widen a value the
  // request has already constrained: the generated schema would permit any string, and the
  // UI would have to parse it back to a number to compare against what it asked for.
  Attribute("window_days", Int32, "Reporting window in days, echoing the request", func() {
    Enum(7, 14, 30)
  })
  Attribute("pulled_at", String, "ISO 8601 timestamp when metrics were pulled")
  Attribute("total_keywords", Int32, "Total keyword count")
  Attribute("keywords", ArrayOf(CampaignKeyword))
  Attribute("totals", func() {
    Attribute("impressions", Int64)
    Attribute("clicks", Int64)
    Attribute("ctr", Float64)
    Attribute("spend_micros", Int64)
    // Float64 for the same reason as the per-keyword field above; a sum of fractional
    // conversions is fractional, and truncating the TOTAL compounds the per-row loss.
    Attribute("conversions", Float64)
    // Totals members are always computed, so they are values, not pointers.
    Required("impressions", "clicks", "ctr", "spend_micros", "conversions")
  })
  Required("campaign_id", "platform_campaign_id", "window_days", "pulled_at", "total_keywords", "keywords", "totals")
})

// Individual action result
var KeywordActionResult = Type("keyword-action-result", func() {
  Attribute("ad_group_id", String, "Ad group ID the action targeted")
  Attribute("criterion_id", String, "The keyword's criterion ID")
  Attribute("keyword", String, "Keyword text (for logging/display)")
  Attribute("action", String, "Action that was performed")
  // THREE states, in a machine-readable field. `success: bool` cannot express the one that
  // matters most — the batch stopped mid-flight and this operation's fate is genuinely
  // unknown — and encoding it in `message` prose forces every consumer to string-match to
  // avoid the wrong next action. An unconfirmed operation reported as `success: false` is a
  // claim the service cannot support, and it is the claim that produces a full-batch retry.
  Attribute("outcome", String, "applied | failed | unconfirmed", func() {
    Enum("applied", "failed", "unconfirmed")
  })
  // success is RETAINED for the existing UI contract and is defined as `outcome ==
  // "applied"` — so an unconfirmed operation is false here too. That is why `failed` below
  // is NOT derived from it. New consumers read `outcome`; `success` exists so the current
  // client keeps compiling, and is the field to drop once the UI migrates.
  Attribute("success", Boolean, "Deprecated: true iff outcome == \"applied\". Read outcome instead")
  Attribute("message", String, "Outcome or error message")
  // message is always populated — a success reason or the failure — so it is a value,
  // not a *string the handler cannot assign to.
  Required("ad_group_id", "criterion_id", "keyword", "action", "outcome", "success", "message")
})

// Response for update-campaign-keywords
var CampaignKeywordActionResult = Type("campaign-keyword-action-result", func() {
  Attribute("total", Int32, "Total actions requested")
  Attribute("succeeded", Int32, "Number with outcome == applied")
  Attribute("failed", Int32, "Number with outcome == failed (does NOT include unconfirmed)")
  // The third count, and the reason `failed` is counted rather than derived. A consumer
  // computing `total - succeeded` folds unconfirmed operations into failures — the exact
  // fold this design exists to prevent, arrived at by arithmetic instead of by a decision.
  // total == succeeded + failed + unconfirmed, always.
  Attribute("unconfirmed", Int32, "Number whose fate is unknown — verify in Google Ads before retrying")
  Attribute("results", ArrayOf(KeywordActionResult), "Per-action outcome")
  Required("total", "succeeded", "failed", "unconfirmed", "results")
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
```

`ErrInvalidBidChange` is **Phase 2** and is declared in the Phase 2 commit, not here — see
[Phase 2 surface](#phase-2-surface-not-in-pr-1). Nothing in Phase 1 can construct it: the
Phase 1 Goa enum is `("pause", "remove")`, so a `change-bid` action is rejected by the
generated validation before any handler runs.

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
  // Phase 2 adds `BidMicros *int64` here, in the same commit as the enum widening. It is
  // deliberately absent in Phase 1: with no bid_micros attribute in the Phase 1 design
  // there is nothing to populate it from, so it would be a field that is always nil and a
  // validation branch no request can reach. See [Phase 2 surface](#phase-2-surface-not-in-pr-1)
  // for why it must be a pointer when it does land.
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
  Conversions    float64  // Google types metrics.conversions as a double; attribution is fractional
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
  Conversions float64  // fractional, as above
}

// KeywordActionState is the three-valued fate of one keyword operation. A bool cannot
// carry the third value, and the third value is the one that changes what an operator does.
type KeywordActionState string

const (
  KeywordActionApplied     KeywordActionState = "applied"
  KeywordActionFailed      KeywordActionState = "failed"
  // KeywordActionUnconfirmed: the operation was in flight when the call failed, or shared a
  // mutate chunk with the operation that was. It MAY have committed upstream. Never retry
  // it blindly; surface it for verification in Google Ads.
  KeywordActionUnconfirmed KeywordActionState = "unconfirmed"
)

// KeywordActionOutcome is the result of one keyword operation.
type KeywordActionOutcome struct {
  AdGroupID   string
  CriterionID string
  Keyword     string
  Action      string
  State       KeywordActionState
  Message     string
}

// Applied reports whether the operation definitely committed. It is what populates the
// legacy `success` boolean on the wire — deliberately a method rather than a second stored
// field, so the two cannot drift apart.
func (o KeywordActionOutcome) Applied() bool { return o.State == KeywordActionApplied }
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
  // Phase 2 inserts the change-bid bid validation here — see
  // [Phase 2 surface](#phase-2-surface-not-in-pr-1). It is not in PR 1 because the Phase 1
  // enum cannot carry "change-bid" and KeywordAction has no BidMicros field, so the loop
  // would be unreachable code guarding a field that does not exist.
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
    WindowDays:       days,
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

// UpdateCampaignKeywords applies bulk keyword actions (Phase 1: pause, remove).
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
      // Phase 2 adds `BidMicros: act.BidMicros` here, alongside the domain field and the
      // widened attribute. It is a straight pointer copy — *int64 on both sides — never a
      // dereference, which would panic for every pause/remove action.
    }
  }
  
  // Call the orchestrator
  outcomes, aerr := orch.UpdateCampaignKeywords(ctx, p.ProjectID, existing.Platform, existing, actions)
  if aerr != nil {
    switch {
    case errors.Is(aerr, ErrKeywordManagerUnsupported):
      return nil, &briefs.BadRequestError{Code: "400", Message: "keyword updates are not supported for this campaign's platform"}
    // Phase 2 adds an `errors.Is(aerr, ErrInvalidBidChange)` arm here returning 400. It is
    // not in PR 1: with no sentinel and no way to submit change-bid, the arm would be dead
    // and a reviewer could not tell whether the mapping was ever exercised.
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
  var succeeded, failed, unconfirmed int32
  for i, outcome := range outcomes {
    results[i] = &briefs.KeywordActionResult{
      AdGroupID:   outcome.AdGroupID,
      CriterionID: outcome.CriterionID,
      Keyword:     outcome.Keyword,
      Action:      outcome.Action,
      Outcome:     string(outcome.State),
      Success:     outcome.Applied(),  // legacy field, derived — never stored twice
      Message:     outcome.Message,
    }
    // Counted per state, NOT derived. `failed := len(outcomes) - succeeded` is the bug this
    // whole three-state design exists to prevent: it silently reclassifies every unconfirmed
    // operation as a failure, and a UI reading that offers a one-click retry of a batch that
    // may already have committed.
    switch outcome.State {
    case model.KeywordActionApplied:
      succeeded++
    case model.KeywordActionFailed:
      failed++
    case model.KeywordActionUnconfirmed:
      unconfirmed++
    }
  }
  
  return &briefs.CampaignKeywordActionResult{
    Total:       int32(len(outcomes)),
    Succeeded:   succeeded,
    Failed:      failed,
    Unconfirmed: unconfirmed,
    Results:     results,
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

So the keyword work is **two layers, not one**, and PR 2 must contain both:

1. `internal/platform/googleads/keywords.go` — two new *semantic* methods on `*Client`:
   `ListCampaignKeywords(ctx, campaignID string, startDate, endDate string) ([]KeywordRow, error)`
   and `MutateKeywordCriteria(ctx, ops []KeywordCriterionOp) ([]KeywordCriterionResult, error)`.
   These own the GAQL text, the resource-name construction, the response decoding, and the
   malformed-response classification — exactly where `CreateCampaign` and `UpdateCampaignStatus`
   already put that work. `KeywordRow` also carries the finished `GoogleAdsURL`, built here from
   the client's own (unexported) customer ID, so the dispatcher never needs an account accessor.

   Because `MutateKeywordCriteria` chunks internally, its error has to say **how far it got**,
   or the caller cannot tell "nothing was applied" from "the first two chunks committed and the
   third failed". A bare error collapses those into one, and they call for opposite operator
   actions. So the failure is typed:

   ```go
   // PartialMutateError reports a mutate that stopped part-way through its chunks.
   //
   // The unit of ambiguity is the CHUNK, not the operation. MutateKeywordCriteria sends a
   // whole chunk in ONE HTTP request, so when a response is lost every operation in that
   // request is unconfirmed — not just the first one. A single AppliedThrough index cannot
   // say that: it makes the operations after it look "never attempted" and therefore safe to
   // retry, when they were in the same in-flight request and may equally have committed.
   // That is the most dangerous possible mislabelling, because it is the one an operator
   // acts on.
   //
   // So the boundaries are recorded as a RANGE, and the three groups are explicit:
   //   ops[:ConfirmedThrough]              — the upstream response confirmed these;
   //                                         Results holds their outcomes, index-aligned
   //   ops[ConfirmedThrough:UnsentFrom]    — the in-flight chunk: UNCONFIRMED, every one
   //   ops[UnsentFrom:]                    — never left this process; safe to retry
   //
   // For a failure on the very first chunk ConfirmedThrough is 0 and UnsentFrom is that
   // chunk's length, which is exactly right: nothing is confirmed and the whole chunk is in
   // doubt.
   type PartialMutateError struct {
     // ConfirmedThrough is the number of operations whose outcome the upstream CONFIRMED.
     ConfirmedThrough int
     // UnsentFrom is the index of the first operation that was never transmitted — i.e. the
     // start of the first chunk after the one that failed.
     UnsentFrom int
     Results    []KeywordCriterionResult
     Err        error
   }

   // Error makes this an error — it is returned through the `error` interface and matched
   // with errors.As at the dispatch boundary, so without this method the code does not
   // compile. The message names the BOUNDARIES rather than counts: the type does not know
   // len(ops), so it cannot count the never-sent tail, and "partial failure" on its own
   // tells an operator nothing about which operations are in doubt.
   func (e *PartialMutateError) Error() string {
     return fmt.Sprintf("keyword mutate partially applied: ops[:%d] confirmed, ops[%d:%d] unconfirmed, ops[%d:] never sent: %v",
       e.ConfirmedThrough, e.ConfirmedThrough, e.UnsentFrom, e.UnsentFrom, e.Err)
   }

   // Unwrap keeps the cause inspectable. This matters more than it looks: the caller has to
   // distinguish a context cancellation from an upstream rejection, and it does that with
   // errors.Is(err, context.Canceled) on the SAME error value it just matched with errors.As.
   // Without Unwrap, wrapping the cause here would hide it.
   func (e *PartialMutateError) Unwrap() error { return e.Err }
   ```

   This is the same discipline the create paths already follow: an ambiguous upstream outcome
   is reported as ambiguous rather than assumed to have failed. Assuming failure is the more
   dangerous default here, because it is the one that invites a blind full-batch retry.
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
// `ad_group_criterion.cpc_bid_micros`, NOT `effective_cpc_bid_micros`. The two are not
// interchangeable: the effective bid is what the auction actually used, and it stays
// populated by inheriting the ad group's bid even when the keyword has no bid of its own.
// Selecting it would make max_cpc_micros non-null for every keyword and quietly destroy
// the one distinction the field exists to carry — "this keyword has its own max CPC" vs.
// "this keyword bids at ad-group level". cpc_bid_micros is the criterion-level bid and is
// absent exactly when none is set, which is the contract below.
//
// The campaign filter is what scopes the read. It is applied here, in the client, so no
// caller can accidentally issue an account-wide keyword query.
const keywordReportQuery = `
  SELECT ad_group_criterion.criterion_id,
         ad_group_criterion.keyword.text,
         ad_group_criterion.keyword.match_type,
         ad_group_criterion.status,
         ad_group_criterion.quality_info.quality_score,
         ad_group_criterion.cpc_bid_micros,
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
      // CpcBidMicros, not the effective bid: nil here MEANS "bids at ad-group level".
      MaxCpcMicros: row.CpcBidMicros,
      // The deep link is built by the client and arrives on the row. It cannot be built
      // here: the URL needs the customer ID, `Client.account` is unexported, and there is
      // no CustomerID() accessor — the same boundary this plan states at lines 739–743.
      // Adding an accessor purely to leak the account outward would widen the client's
      // surface for one string; returning the finished URL keeps account-awareness inside
      // the package that already owns it.
      GoogleAdsURL: row.GoogleAdsURL,
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

  // outcomes is index-aligned with `actions` and returned whole. `ops`/`pending` hold only
  // the subset that survives authorization and action-vocabulary checks, so `pending[i]`
  // corresponds to `ops[i]` and to the upstream result at the same index.
  outcomes := make([]*model.KeywordActionOutcome, 0, len(actions))
  ops := make([]googleads.KeywordCriterionOp, 0, len(actions))
  pending := make([]*model.KeywordActionOutcome, 0, len(actions))
  for _, action := range actions {
    ref := googleads.KeywordCriterionRef{AdGroupID: action.AdGroupID, CriterionID: action.CriterionID}
    outcome := &model.KeywordActionOutcome{
      AdGroupID:   action.AdGroupID,
      CriterionID: action.CriterionID,
      Action:      action.Action,
      Keyword:     allowed[ref], // the keyword text, resolved during authorization
    }

    if _, ok := allowed[ref]; !ok {
      outcome.State = model.KeywordActionFailed
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
      outcome.State = model.KeywordActionFailed
      outcome.Message = fmt.Sprintf("unsupported action: %s", action.Action)
      outcomes = append(outcomes, outcome)
      continue
    }

    // Collect; the mutate is issued ONCE, after the loop. See below.
    ops = append(ops, op)
    pending = append(pending, outcome)
    outcomes = append(outcomes, outcome)
  }

  // ONE mutate for every operation, with partial failure enabled.
  //
  // An earlier draft sent one HTTP call per action, reasoning that a batched mutate is
  // atomic so a single bad criterion would report every other action as failed. That is
  // true only with partial failure OFF. With `partialFailure: true` Google Ads applies
  // each operation independently and returns a per-operation error at its own index, which
  // is exactly the attribution the outcome array needs — so the serial version bought
  // nothing and cost a great deal: the documented scale is 100 actions, and 100 sequential
  // round trips will not fit the 30s keywordCallTimeout and burns 100x the API quota for
  // one user gesture. MutateKeywordCriteria chunks internally at the upstream operation
  // limit; the results slice stays index-aligned with ops across chunks.
  res, merr := client.MutateKeywordCriteria(ctx, ops)
  switch {
  case merr != nil:
    // A whole-call failure is UNCONFIRMED, not "nothing happened". Two reasons, and both
    // matter for what the operator does next:
    //
    //   1. `transportError` means the request left and the response did not come back
    //      intact. The mutate may well have committed upstream.
    //   2. MutateKeywordCriteria chunks internally, so a failure on chunk 3 leaves
    //      chunks 1 and 2 DEFINITELY applied. Those operations succeeded and the caller
    //      has no way to learn it from this branch.
    //
    // Marking every pending action `failed` claims none was applied, and an operator reading
    // that retries the whole batch — re-pausing keywords that are already paused (harmless)
    // or, in Phase 2, overwriting an intervening manual bid change with a stale value (not
    // harmless). This mirrors the create-path discipline the dispatchers already use: an
    // ambiguous outcome is reported as ambiguous.
    //
    // So the client returns what it knows, as a RANGE rather than a single index — see
    // PartialMutateError. The chunk is the unit of ambiguity: everything in the in-flight
    // HTTP request is unconfirmed together, because one lost response loses the fate of
    // every operation it carried. Three groups, and the middle one is a span, not a point:
    //   - [0, ConfirmedThrough)          → upstream said OK/failed; keep its verdict
    //   - [ConfirmedThrough, UnsentFrom) → the in-flight chunk: UNCONFIRMED, all of it
    //   - [UnsentFrom, len)              → never transmitted; safe to retry
    //
    // When the error is NOT a *PartialMutateError the client could not say how far it got
    // by chunk index — but that does NOT mean the outcome is ambiguous. Two large classes
    // of non-partial error are provably "nothing was applied":
    //
    //   - PRE-SEND failures. Credential resolution, account validation, request
    //     construction, a context already cancelled — no HTTP request left this process,
    //     so no keyword changed.
    //   - DEFINITE upstream REJECTIONS. `partialFailure: true` makes Google apply the
    //     operations INDEPENDENTLY and report per-operation errors at their own index —
    //     but only once the request itself is accepted. A request-level 4xx (expired
    //     credential, malformed request, a quota refusal) is a rejection of the CALL, so
    //     no operation in it was evaluated and none committed. Partial failure changes
    //     how a bad OPERATION is reported; it does not make a rejected REQUEST partly
    //     apply.
    //
    // Blanket-marking those `unconfirmed` is not conservative, it is wrong in the
    // expensive direction: it tells the operator "check Google Ads before retrying" for
    // an operation that certainly did not happen, and the three-state design exists
    // precisely so `unconfirmed` stays rare enough to be worth acting on. An `unconfirmed`
    // that fires on every expired credential is noise, and noise gets ignored — including
    // the one time it was real.
    //
    // So the fallback asks the client, which already owns this classification:
    // `googleads.IsOutcomeUnconfirmed` (client.go / campaign.go) reports true for
    // transport failures, 5xx, 429-after-send, cancellation mid-flight, and malformed 2xx —
    // and false for pre-send and definite-rejection errors. Same helper the create and
    // toggle paths use, so keyword writes cannot drift from the rest of the dispatcher.
    //
    // The *PartialMutateError branch asks the SAME question, and an earlier draft of this
    // plan got that wrong. It marked the whole in-flight span `unconfirmed` unconditionally,
    // reasoning that the terminal error describes only the last chunk. But the span
    // `[ConfirmedThrough, UnsentFrom)` IS the last chunk — that is what `ConfirmedThrough`
    // means. Earlier chunks already carry their own verdicts from `pe.Results`, and
    // operations past `UnsentFrom` never left. So the terminal error speaks to exactly the
    // span, and a definite rejection of that one request makes the span `failed`, not
    // `unconfirmed`. The classification is the same helper in both branches; only the RANGE
    // it applies to differs, which is the whole reason the client encodes the range.
    //
    // The platform client already classifies and redacts upstream errors; do not
    // re-render the raw response body into a user-visible message.
    var pe *googleads.PartialMutateError
    confirmedThrough, unsentFrom := 0, len(pending)
    ambiguous := googleads.IsOutcomeUnconfirmed(merr)
    if errors.As(merr, &pe) {
      confirmedThrough, unsentFrom = pe.ConfirmedThrough, pe.UnsentFrom
      for i, r := range pe.Results {
        if r.OK {
          pending[i].State = model.KeywordActionApplied
        } else {
          pending[i].State = model.KeywordActionFailed
        }
        pending[i].Message = r.Message
      }
    } else if !ambiguous {
      // Nothing reached Google, or Google rejected the whole batch. Every operation
      // failed, and retrying after fixing the cause is safe.
      for _, outcome := range pending {
        outcome.State = model.KeywordActionFailed
        outcome.Message = "not applied: the request was rejected before any keyword was " +
          "changed — fix the cause and retry the whole batch"
      }
      break
    }
    for i := confirmedThrough; i < unsentFrom; i++ {
      if !ambiguous {
        // Google received this chunk and rejected the request. Nothing in it was
        // evaluated, so "check Google Ads" would send the operator to look at a change
        // that provably did not happen.
        pending[i].State = model.KeywordActionFailed
        pending[i].Message = "not applied: this keyword was in the request Google rejected — " +
          "fix the cause and retry it"
        continue
      }
      pending[i].State = model.KeywordActionUnconfirmed
      pending[i].Message = "unconfirmed: this keyword was in the request that failed after " +
        "it was sent, so it may or may not have been changed — check Google Ads before retrying"
    }
    for i := unsentFrom; i < len(pending); i++ {
      pending[i].State = model.KeywordActionFailed
      pending[i].Message = "not attempted: an earlier part of this batch failed before this " +
        "keyword was sent"
    }
  case len(res) != len(ops):
    // A 2xx whose result count does not match the operation count is a malformed
    // response, not a success. Indexing res[i] under that assumption — as an earlier
    // draft did, and again into Errors[0] — panics the request instead of producing a
    // controlled platform error. Length checks belong in the client, which is why
    // MutateKeywordCriteria returns a typed slice and validates the count itself; this
    // is the caller-side backstop.
    // UNCONFIRMED, not failed: Google answered 2xx, so the mutate very likely committed —
    // we simply cannot map results back to operations. Calling that a failure invites the
    // retry.
    for _, outcome := range pending {
      outcome.State = model.KeywordActionUnconfirmed
      outcome.Message = "google ads returned an incomplete result set for this batch; the " +
        "changes may have been applied — check Google Ads before retrying"
    }
  default:
    for i, outcome := range pending {
      if res[i].OK {
        outcome.State = model.KeywordActionApplied
      } else {
        outcome.State = model.KeywordActionFailed
      }
      outcome.Message = res[i].Message
    }
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

**Note on `LAST_30_DAYS` semantics — this CHANGES the reporting window.** Google's own
`LAST_30_DAYS` excludes today, and so does the current UI: `resolveDateRange` maps `days` to the
GAQL literals `LAST_7_DAYS` / `LAST_14_DAYS` / `LAST_30_DAYS`
(`campaign-metrics.service.ts:128-132`), all of which end YESTERDAY. The window computed above
*includes* the partial current day, so the same `days=30` request returns a different set of
rows than the screen it is meant to replace — numbers will not tie out against the existing view
during any overlap.

That may still be the right call: the keyword table is used for intra-day decisions, and a
30-day window that silently stops at midnight is a real complaint. But it is a **behaviour
change requiring an explicit decision and a test**, not a note. Open Question 4 as originally
written asked only which day COUNTS to allow, which is the smaller question; it now also has to
answer include-today vs. completed-days. Whichever way it lands, pin it:

- If completed-days wins, `dateRangeForDays` ends at `now-1` and a test asserts the end date is
  not today.
- If include-today wins, a test asserts the end date IS today, and the migration note says
  plainly that keyword numbers will differ from the legacy endpoint by one partial day.

Either way the divergence from `MetricsWindow`'s Google-aligned semantics elsewhere in this
service is documented at the endpoint, so a reader comparing two surfaces is not left guessing.

**File to edit:** `internal/dispatch/status_toggler_guard_test.go` — **not** `googleads.go`.

**Declare that `GoogleAdsDispatcher` satisfies the KeywordManager capability**, in the existing
guard's `var` block:

```go
var (
	_ service.StatusToggler = (*RedditDispatcher)(nil)
	// … the five other togglers …
	_ service.KeywordManager = (*GoogleAdsDispatcher)(nil)
)
```

The interface is declared in `internal/service` (the orchestrator's package), matching where
`MetricsReader` lives. That is precisely why the assertion cannot live in `googleads.go`:
`internal/dispatch` must not import `internal/service` in production code, and
`status_toggler_guard_test.go:25-27` exists to say so — *"a test file so the service import
stays test-only, avoiding any production dispatch→service dependency"*. Putting
`var _ service.KeywordManager = …` in `googleads.go` would create exactly the production
`dispatch → service` edge that guard was written to prevent, and would do it silently, since
the import already resolves.

Co-locating it with the togglers also keeps one place to look. The reason the assertion is
needed is identical: `Orchestrator.UpdateCampaignKeywords` discovers the capability via a
RUNTIME `d.(KeywordManager)` assertion, so a drifting `ListKeywords`/`UpdateKeywords`
signature would not fail the build — it would silently stop satisfying the interface and
every keyword call would return `ErrKeywordManagerUnsupported` (a 400) instead of working.
Rename the file to `capability_guard_test.go` in the same commit if the toggler-specific name
becomes misleading; the guard is no longer toggler-only.

---

## PR Breakdown

> **On the `Base:` fields below.** These are written as a stack (each PR based on the previous),
> which conflicts with the repo's standing convention that **every PR targets `main`**. The Google
> Ads stack (#66→#67→#68→#69→#70) is the cautionary case: it stalled for days because #69
> conflicted against its BASE rather than against `main`, and each unmerged parent multiplied the
> rebase cost of everything above it. That particular jam has since cleared — #69 merged on
> 2026-08-07 — but the cost it demonstrated is the reason for the rule, not an argument that the
> rule was unnecessary.
>
> Every PR below therefore targets `main` and is opened only once its predecessor has merged. The
> ordering is a **build order**, not an instruction to open a chained stack up front.

> **Design and handlers cannot be separate PRs.** An earlier draft split them, and that split does
> not build. `make apigen` puts `ListCampaignKeywords` and `UpdateCampaignKeywords` on the
> generated `briefs.Service` interface, and `internal/service/brief.go:48` asserts
> `var _ briefs.Service = (*BriefService)(nil)` at compile time. The moment the design lands
> without the handlers, that assertion fails and `go build ./...` is red on `main` — the design
> PR is not "design-only", it is a breaking change to an interface the service already claims to
> satisfy. The two are merged into PR 1 below. This is a general property of Goa in this repo, not
> a quirk of keywords: **any PR that adds a method to a service design must implement it.**

### PR 1: Goa Design + Domain Model + Orchestrator + Handlers (≈500 lines)

**Branch:** `feat/LFXV2-2023-keyword-surface`
**Base:** `origin/main`
**Files:** `design/brief.go`, `internal/domain/model/keyword.go`, `internal/domain/errors.go`,
`internal/service/orchestrator.go`, `internal/service/brief.go`, `internal/service/brief_test.go`,
`internal/service/orchestrator_test.go`

- Add Goa type definitions for keyword list/action payloads and results
- Add new methods `list-campaign-keywords`, `update-campaign-keywords`
- Define error sentinels in `domain/errors.go`; add `model/keyword.go`
- Run `make apigen` to regenerate `gen/` **and** the ko-embedded OpenAPI copies under
  `cmd/campaign-service/kodata/gen/http/`. Not `goa gen` alone (leaves kodata stale) and
  definitely not `go run ./cmd/okfgen`, which regenerates the knowledge bundle.
- Add `KeywordManager` interface + `ReadCampaignKeywords` / `UpdateCampaignKeywords` to Orchestrator
- Implement `ListCampaignKeywords` / `UpdateCampaignKeywords` handlers in `BriefService` —
  **required in this PR**, per the note above
- Add timeouts + error mapping
- Add unit tests for error cases (unsupported platform, unprovisioned campaign, empty actions array)

No dispatcher implements `KeywordManager` yet at this point, which is the correct intermediate
state and not a gap: the capability is optional, so both endpoints return a clean
`ErrKeywordManagerUnsupported` → 400 until PR 2 wires Google Ads. That is exactly how
`get-campaign-metrics` behaved between its foundation PR and the first platform adapter.

**Test cases to add:**
- `TestBriefService_ListCampaignKeywords_HappyPath`
- `TestBriefService_ListCampaignKeywords_PlatformUnsupportedIs400`
- `TestBriefService_ListCampaignKeywords_PlatformFailureIs503`
- `TestBriefService_UpdateCampaignKeywords_HappyPath`
- `TestBriefService_UpdateCampaignKeywords_PlatformUnsupportedIs400`
- `TestBriefService_UpdateCampaignKeywords_EmptyActionsIs400`
- `TestBriefService_UpdateCampaignKeywords_PlatformFailureIs503`

### PR 2: Google Ads Keyword Operations — Phase 1 (≈750 lines)

**Branch:** `feat/LFXV2-2023-keyword-googleads-phase1`
**Base:** `origin/main`, opened once PR 1 has merged
**Files:** `internal/platform/googleads/keywords.go` **(new — this PR must include the platform
client, not just the adapter)**, `internal/platform/googleads/keywords_test.go`,
`internal/dispatch/googleads.go` (add `ListKeywords`/`UpdateKeywords` — **methods only**),
`internal/dispatch/status_toggler_guard_test.go` (the `KeywordManager` assertion goes HERE, so the
`internal/service` import stays test-only — see "File to edit" above),
`internal/dispatch/googleads_test.go`

- Platform client: `ListCampaignKeywords`, `AuthorizeKeywordCriteria`, `MutateKeywordCriteria`
  (single batched mutate with `partialFailure: true`, chunked at the upstream operation limit),
  composite resource-name construction, the `GoogleAdsURL` deep link, GAQL text (snake_case,
  campaign-scoped), response length validation
- Dispatcher: model translation, totals accumulation with a guarded CTR, per-action outcomes
- Add date-range parsing for days ∈ {7, 14, 30} (inclusive, single clock read)
- Use `d.resolveGoogleAdsClient` (`internal/dispatch/googleads.go:202`); `ToggleStatus` (`:252`) is the closest existing caller to model on

**Test cases:**
- `TestClient_ListCampaignKeywords_QueryIsSnakeCaseAndCampaignScoped` (pins the GAQL text —
  camelCase paths are rejected upstream and no unit test would otherwise notice)
- `TestClient_KeywordCriterionResourceName_IsComposite`
- `TestClient_MutateKeywordCriteria_ShortResultSetIsErrorNotPanic`
- `TestClient_MutateKeywordCriteria_SendsOneRequestForManyOperations` (pins the batch:
  a regression to one-call-per-action passes every behavioural test but blows the budget)
- `TestDispatcher_UpdateKeywords_PartialFailureAttributesPerAction`
- The four error classes must be asserted SEPARATELY — a single "the mutate failed" test
  passes whether the fallback classifies correctly or blanket-marks everything unconfirmed:
  - `TestDispatcher_UpdateKeywords_PreSendFailureIsFailedNotUnconfirmed` — make credential
    resolution fail, so no HTTP request is issued; every outcome must be `failed`
  - `TestDispatcher_UpdateKeywords_DefiniteRejectionIsFailedNotUnconfirmed` — serve a 400
    `INVALID_ARGUMENT`; the batch is atomic, so every outcome must be `failed`
  - `TestDispatcher_UpdateKeywords_TransportFailureIsUnconfirmed` — close the connection
    mid-response; every outcome must be `unconfirmed`
  - `TestDispatcher_UpdateKeywords_LaterChunkFailureLeavesEarlierChunksConfirmed` — more
    operations than one chunk holds, first chunk 2xx, second fails; the first chunk's
    outcomes keep their upstream verdicts and only the second chunk's are `unconfirmed`
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

### PR 3: Integration Tests (≈200 lines)

**Branch:** `feat/LFXV2-2023-keyword-integration-phase1`
**Base:** `origin/main`, opened once PR 2 has merged
**Files:** `internal/service/brief_test.go` (new E2E-style tests) — **tests only**

> **No generated specs in this PR.** An earlier draft assigned "the OpenAPI snapshot" here.
> That is not deferrable: PR 1 changes `design/brief.go` and therefore runs `make apigen`, and
> `make apigen` writes BOTH generated trees — `gen/` and the ko-embedded copies under
> `cmd/campaign-service/kodata/gen/http/`. Committing only one of them leaves the repo in a
> state where re-running `make apigen` produces a diff, which CI treats as drift, and leaves
> the binary serving an OpenAPI document that does not describe its own endpoints. Both trees
> ship with the contract change in PR 1; there is no snapshot left over for PR 3.

- Full integration test: create campaign → list keywords → pause some → verify result
- Uses real Goa-generated types and service handlers (no mocks at this level)
- Mock the Orchestrator's dispatcher map to inject a test-only KeywordManager
- Verify HTTP status codes and response shapes

**Test cases:**
- `TestIntegration_ListKeywordsEndpoint`
- `TestIntegration_UpdateKeywordsEndpoint`
- `TestIntegration_KeywordErrorHandling`

**Lines:** ~200 (tests only)

---

## Test Plan

### Unit Tests (Per-Layer)

1. **Domain Model** (model/keyword.go)
   - Verify KeywordAction/KeywordMetric struct construction (minimal; mostly data containers)

2. **Orchestrator Interfaces**
   - Mock KeywordManager for both ListKeywords and UpdateKeywords
   - Verify timeout context is passed correctly
   - Verify error sentinel mapping (ErrKeywordManagerUnsupported → 400)
   - (Phase 2) Verify conditional bid validation: change-bid requires positive BidMicros

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
  dispatched, so a locally-present, upstream-absent campaign is an ordinary state, not an
  impossible one. Without the guard the GAQL query carries an empty campaign filter and reads
  the whole account.

  Do **not** rest that on "`platform_campaign_id` stays empty until the platform call
  succeeds" — it does not. `docs/knowledge/code/internal-service.md:37-41` records that a
  retained `pending` partial can carry an upstream id: the create succeeded, a later step in
  the same dispatch did not, and the row is kept as a reconciliation-required ORPHAN
  precisely so the id is not lost. The real invariant is narrower and is the one the guard
  actually needs: **the id is empty until the upstream campaign exists**, in either
  direction. Non-empty ⇒ there is a Google Ads campaign to address.

  That leaves a genuine decision, which this plan makes rather than leaves implicit:

  **Keyword writes are allowed whenever `platform_campaign_id` is non-empty, including for
  `pending` and reconciliation-required campaigns.** The upstream campaign exists, its
  keywords exist, and an operator pausing a runaway keyword on a half-provisioned campaign is
  exactly who needs this endpoint most — gating on `status == created` would refuse the call
  in the one state where spend is least supervised. Nothing about a pending row makes the
  criterion mutate less safe: `AuthorizeKeywordCriteria` still scopes every criterion to this
  campaign, so a partial row cannot be used to reach another campaign's keywords.

  Tests, both of which fail under either mistaken invariant:
  - `TestOrchestrator_UpdateCampaignKeywords_PendingWithUpstreamIDIsAllowed` — status
    `pending`, `platform_campaign_id` set; must dispatch, not 409
  - `TestOrchestrator_UpdateCampaignKeywords_CreatedWithoutUpstreamIDIs409` — status
    `created`, id empty; must 409, not send an unscoped GAQL query

  The same rule and the same pair of tests apply to the read path
  (`ReadCampaignKeywords`), whose guard is character-for-character the same.

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

Eight were raised; **four are resolved inside this plan** (2 keyword text, 5 criterion id vs
resource name, 6 partial success, 7 Phase 2 timing) and are kept below with their resolutions
so a reviewer can check the reasoning rather than re-open them. **Four remain genuinely open**
and need a human: 1 (bid currency — Phase 2 only), 3 (pagination), 4 (metrics window
alignment), and 8 (the identity gap, which is the only one that changes the delivery shape).

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

**Decision Needed (a) — which day counts to accept:**
- Keep the strict enum (breaks compatibility with any UI call using days=1 or days=60)?
- Map arbitrary days to the nearest enum value (days ≤ 7 → 7, 7 < days ≤ 14 → 14, days > 14 → 30)?
- **Recommendation:** Keep strict enum. The UI should be updated to only call with valid values. The current `/api/campaigns/keywords` BFF endpoint accepts arbitrary `days` but `resolveDateRange` already collapses it to one of three GAQL literals, so the enum loses nothing a caller can actually observe.

**Decision Needed (b) — where the window ENDS.** This is the larger half and was missing from the
original framing. The UI's `resolveDateRange` (`campaign-metrics.service.ts:128-132`) emits
`LAST_7_DAYS` / `LAST_14_DAYS` / `LAST_30_DAYS`, all of which END YESTERDAY. The explicit-date
computation proposed above ends TODAY. Same `days=30`, different rows.

- Preserve completed-day semantics (end at `now-1`): numbers tie out against the existing screen;
  intra-day changes are invisible until the next day.
- Include today: better for the optimize loop this surface exists for, but keyword numbers will
  not reconcile with the legacy endpoint, and that has to be stated in the UI.
- **Recommendation:** preserve completed-day semantics for the first cut, because "the new page
  disagrees with the old page" is the failure mode that costs the most trust, and switching to
  include-today later is additive. **Either choice ships with a test asserting the end date**, so
  the window cannot drift silently.

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

**Recommendation:** Keep partial-success (200 + per-action outcomes). The UI can show a summary
("95 succeeded, 5 failed") and let the user retry the failed ones.

**But two outcomes, not three, is the actual trap here.** `success: true|false` cannot express the
case that matters most: the batch stopped mid-flight and one operation's fate is genuinely unknown.
Reporting that operation as `success: false` is a claim the service cannot support, and it is the
claim that produces the worst next action — a full-batch retry that re-applies whatever already
committed.

Note what the retry risk actually is, because the obvious framing is wrong. `change-bid` sets an
ABSOLUTE `bid_micros` (see the operation shape above), so replaying it does NOT stack a second
adjustment — it writes the same number again, which on its own is idempotent. The real hazard is
**overwriting an intervening change**: between the ambiguous first attempt and the retry, someone
can raise the bid in Ads Manager or an automated rule can move it, and the blind replay silently
reverts that to the value the batch was originally built from. `remove` has a companion hazard —
a replay of an already-applied `remove` returns a not-found error that reads as a NEW failure and
sends the operator looking for a problem that does not exist. Neither is caught by "the operation
is idempotent"; both are caught by not retrying an operation whose fate is unknown.

The `merr != nil` branch above therefore distinguishes *unconfirmed* from *not attempted*, and the
UI should surface the unconfirmed one as "check Google Ads" rather than folding it into the failed
count for a one-click retry.

**Resolved in this design:** `KeywordActionResult` carries an explicit three-state `outcome`
(`applied` / `failed` / `unconfirmed`) rather than encoding the distinction in prose, with the
boolean `success` retained as a deprecated derived field so the existing UI shape does not break —
see the Goa result type and `model.KeywordActionState` above. Encoding a state the UI must branch
on inside a human-readable `message` would make correct UI behaviour depend on string matching.

### 7. Phase 2 Timing & Dependency on PR #69

**Resolved — no longer a question.** PR #69 (GA-4 targeting) merged to `main` on 2026-08-07 at
15:37 UTC and `internal/platform/googleads/targeting.go` is present. Phase 2's soft dependency
on its keyword resource-name machinery is satisfied by branching from current `main`. The
options below are retained for the record; the phasing choice is now about review size alone.

**Decision Needed:**
- Should Phase 1 block until #69 is merged, or land independently and Phase 2 chains after?
- ~~If Phase 1 lands first, should it pre-implement enable/change-bid (disabled with "not yet" error) to avoid a second API change?~~ **Answered: no.** The Phase 1 enum cannot carry the actions, so any pre-implemented validation, sentinel, or switch arm is unreachable and untestable. See [Phase 2 surface](#phase-2-surface-not-in-pr-1) for the full list of what moves.
- **Recommendation:** Land Phase 1 independently. Phase 2 is a separate PR off current `main`. Do NOT pre-implement Phase 2 in Phase 1.

### 8. The identity gap — how the account-wide UI view migrates to a campaign-scoped endpoint

**This is the one open question that changes the delivery shape, not just a field.** The existing
BFF `getKeywords` (`campaign-metrics.service.ts:67-95`) runs ONE account-wide GAQL query over
`keyword_view` with no project, brief, or campaign scope, and returns Google Ads
`campaign.id` / `ad_group.id` strings. The endpoint designed here is scoped to a
campaign-service campaign UUID. They do not return the same set of rows, so this is not a URL
swap — see "The identity gap" section above.

**Decision Needed** — pick one:
- **Scope the view.** The UI adds a campaign selector and calls the new endpoint per campaign.
  Simplest server side; changes the screen's information architecture.
- **Resolve GA ids to local campaigns.** Add an account-wide read that maps each returned
  `campaign.id` back through `PlatformCampaignID` so the flat table survives. Preserves the
  screen; needs a second endpoint and a decision about keywords on campaigns this service does
  not own.
- **Recommendation:** scope the view. The keyword surface exists to act on one campaign's
  keywords, and an account-wide table that mixes campaigns this service cannot act on is a
  worse starting point than a narrower one that is correct.

Until this is decided, the new endpoint is **additive**: `GET /api/campaigns/keywords` is not
retired by PR 1.

---

## Knowledge Base Updates

**This PR carries its own.** `CLAUDE.md:17-26` requires every merged PR to update the relevant
concept and add a dated log fragment; deferring that to PR 1 would put this one outside the merge
contract. The plan is itself a durable decision record, so it ships with:

- `docs/knowledge/log/2026-08-06-lfxv2-2023-keyword-surface-plan.md` — what was decided and, more
  usefully, the contract facts the first drafts got wrong (GAQL is snake_case; criterion resource
  names are composite; `make apigen` not `okfgen`; optional Goa attributes generate pointers while
  DEFAULTED ones generate values, so a `Default()` and a `!= nil` handler check cannot both be
  right; a
  Goa design PR cannot land without its handlers; `cpc_bid_micros` is not
  `effective_cpc_bid_micros`; the bulk-mutation rule needs answering, not citing).

Then, as each implementation PR lands:

1. `docs/knowledge/code/design.md` and `docs/knowledge/code/internal-service.md` — the
   two new methods, their types, and the `KeywordManager` optional capability (PR 1)
2. `docs/knowledge/code/internal-platform-googleads.md` — the keyword client surface, the GAQL
   query, and criterion authorization (PR 2)
3. `docs/api-catalog.md` — the two endpoint rows, with the `keyword-actions` row updated from
   planned to implemented
4. A dated `docs/knowledge/log/` fragment per PR

---

## Conclusion

This plan provides a concrete, layered implementation roadmap for the Optimize-phase keyword surface. **Phase 1 (pause/remove) is deliverable independently and immediately.** Phase 2 (enable/change-bid) is equally unblocked — #69 merged on 2026-08-07, so its resource-name machinery is already on `main`; splitting the phases is now a review-size choice, not a dependency. Each PR is under 1000 hand-written lines and reviewable against clear acceptance criteria.

**Dependencies and blocking factors are resolved:**
- No dependency on PR #69 for either phase — it merged 2026-08-07
- Error mapping follows established patterns (MetricsReader, StatusToggler)
- The Google Ads surface is reached through `internal/platform/googleads`, which PR 2 extends
  with keyword read/mutate/authorize methods — there is no SDK and no exported search or mutate
  entry point to call from the dispatcher
- Test plan covers all error cases and happy paths

**Open questions are _design choices_, not blockers** — they can be decided by the owning team and implemented in parallel with code review.
