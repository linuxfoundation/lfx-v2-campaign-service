# Campaign Service — API & Platform Catalog

Reference catalog of all campaign endpoints, platform account attributes, and data structures. This documents the current Express BFF implementation that will be ported to the Go service.

## API Endpoints

Base path: `/api/campaigns`

### Brief Generation (Planning Phase)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| POST | `/brief/generate` | SSE stream | Generate a new brief or refresh an existing one. If a brief already exists for this project + event slug, it updates and bumps the version. |
| POST | `/brief/refine` | SSE stream | Refine existing brief based on user feedback |
| GET | `/projects/:projectId/briefs` | JSON | List all saved briefs for a project |
| GET | `/projects/:projectId/briefs/:id` | JSON | Get a specific brief (full copy, keywords, targeting) |
| PUT | `/projects/:projectId/briefs/:id` | JSON | Manually edit a saved brief (copy, keywords, targeting) |
| POST | `/projects/:projectId/briefs/:id/approve` | JSON | Approve a brief for campaign creation. Records who approved and when. |
| GET | `/projects/:projectId/briefs/:id/versions` | JSON | Get version history for a brief (what changed, who changed it, when, which campaigns were affected) |
| DELETE | `/projects/:projectId/briefs/:id` | JSON | Archive a brief (soft delete) |

Briefs are persisted per project and keyed by event slug. When a user returns to the same event, the existing brief is loaded for review. They can refresh it (re-run AI generation with latest event data) or manually edit it. Once reviewed and approved, the brief can be used to create campaigns. All campaigns created from a brief are saved as execution records linked back to the brief, including campaign name, platform, budget allocated, and who created them.

### Campaign Creation (Implementation Phase)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| POST | `/create` | JSON | Create or update campaigns across selected platforms (async, returns jobId). If campaigns already exist for this brief, updates them on the platform instead of creating new ones. Saves execution record with who created/updated, budget, dates, and platform details. |
| GET | `/jobs/:jobId` | JSON | Poll campaign creation job status |
| GET | `/projects/:projectId/executions` | JSON | List all campaign executions for a project |
| GET | `/projects/:projectId/executions/:id` | JSON | Get a specific execution with full details |
| GET | `/projects/:projectId/executions/:id/audit` | JSON | Get audit history for a campaign (budget changes, status changes, who made each change) |

### Monitoring (Insights Phase)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| GET | `/monitor` | JSON | Google Ads campaign metrics (days param, default 14) |
| GET | `/keywords` | JSON | Google Ads keyword performance (top 50 by impressions) |
| GET | `/audience` | JSON | Audience demographics (age, gender, device) |
| GET | `/linkedin/accounts` | JSON | List linked LinkedIn ad accounts |
| GET | `/linkedin/monitor` | JSON | LinkedIn campaign metrics (accountKey, days) |
| GET | `/reddit/accounts` | JSON | List configured Reddit ad accounts |
| GET | `/reddit/monitor` | JSON | Reddit campaign metrics (accountKey, days) |
| GET | `/meta/accounts` | JSON | List configured Meta ad accounts |
| GET | `/meta/monitor` | JSON | Meta campaign metrics (accountKey, days) |
| GET | `/twitter/accounts` | JSON | List configured X/Twitter ad accounts |
| GET | `/twitter/monitor` | JSON | X/Twitter campaign metrics (accountKey, days) |

### HubSpot UTM Integration

| Method | Path | Type | Description |
|--------|------|------|-------------|
| GET | `/hubspot/utm` | JSON | Lookup HubSpot campaign by event name |
| POST | `/hubspot/utm/create` | JSON | Create HubSpot campaign if not found |

### Optimization (current)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| POST | `/keywords/actions` | JSON | Bulk pause/remove Google Ads keywords |
| PATCH | `/:campaignId/status` | JSON | Toggle campaign ACTIVE/PAUSED (Meta, Reddit, X) |

### Optimization (tentative)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| PATCH | `/:campaignId/budget` | JSON | Adjust daily or lifetime budget |
| PATCH | `/:campaignId/bid-strategy` | JSON | Change bid strategy (maximize clicks, target CPA, target ROAS) |
| PATCH | `/:campaignId/keywords/:keywordId/bid` | JSON | Update individual keyword bid (Google Ads) |
| POST | `/:campaignId/ads/rotate` | JSON | Pause underperforming ad variants, activate new ones |
| PATCH | `/:campaignId/ads/:adId/copy` | JSON | Update ad copy (headlines, descriptions) without recreating |
| POST | `/:campaignId/creatives/rotate` | JSON | Rotate creatives (LinkedIn, Meta) |
| PATCH | `/:campaignId/geo-targets` | JSON | Add or remove geo targets |
| PATCH | `/:campaignId/audience` | JSON | Update audience segments (skills, groups, interests) |
| POST | `/:campaignId/negative-keywords` | JSON | Add negative keywords (Google Ads) |
| PATCH | `/:campaignId/bid-modifiers` | JSON | Adjust age, gender, or device bid modifiers |
| PATCH | `/:campaignId/schedule` | JSON | Set ad scheduling / dayparting |
| PATCH | `/:campaignId/flight-dates` | JSON | Extend or shorten campaign flight dates |
| POST | `/bulk/status` | JSON | Bulk status toggle across all platforms |
| POST | `/budget/reallocate` | JSON | Reallocate budget across platforms based on performance |

### Platform Connection Management (new endpoints for Go service)

| Method | Path | Type | Description |
|--------|------|------|-------------|
| POST | `/projects/:projectId/connections` | JSON | Create a new platform connection |
| GET | `/projects/:projectId/connections` | JSON | List all connections for a project |
| GET | `/projects/:projectId/connections/:id` | JSON | Get a specific connection |
| PUT | `/projects/:projectId/connections/:id` | JSON | Update a connection |
| DELETE | `/projects/:projectId/connections/:id` | JSON | Remove a connection |

---

## Platform Account Attributes

Each platform connection stores the account identifiers needed to interact with that platform's API.

### Google Ads

| Field | Type | Description |
|-------|------|-------------|
| `customer_id` | string | Google Ads customer ID (10-digit, e.g. `8666746580`) |
| `login_customer_id` | string | Manager account ID used for API access |

### LinkedIn Ads

| Field | Type | Description |
|-------|------|-------------|
| `ad_account_id` | string | LinkedIn ad account ID (e.g. `538170226`) |
| `org_id` | string | LinkedIn organization ID (e.g. `208777`) |

### Meta Ads

| Field | Type | Description |
|-------|------|-------------|
| `ad_account_id` | string | Meta ad account ID |

### Reddit Ads

| Field | Type | Description |
|-------|------|-------------|
| `ad_account_id` | string | Reddit advertiser account ID (e.g. `t2_gv9wtbfa`) |

### X/Twitter Ads

| Field | Type | Description |
|-------|------|-------------|
| `account_id` | string | X Ads account ID (e.g. `8r7gb`) |
| `funding_instrument_id` | string | Funding instrument ID for the account |

### HubSpot

| Field | Type | Description |
|-------|------|-------------|
| `portal_id` | string | HubSpot portal/account ID |

---

## Campaign Platforms

| Platform | Key | Status | Auth Type |
|----------|-----|--------|-----------|
| Google Ads | `google-ads` | Implemented | OAuth 2.0 |
| LinkedIn Ads | `linkedin-ads` | Implemented | OAuth 2.0 |
| Meta Ads | `meta-ads` | Implemented | Bearer token |
| Reddit Ads | `reddit-ads` | Implemented | OAuth 2.0 |
| X/Twitter Ads | `twitter-ads` | Implemented | OAuth 1.0a (HMAC-SHA1) |
| Microsoft Ads | `microsoft-ads` | Not yet implemented | — |

---

## Campaign Types

### Program Types

| Type | Description |
|------|-------------|
| `events` | Conference/summit campaigns (e.g., KubeCon, All Systems Go) |
| `education` | Training/certification campaigns (e.g., CKA, LFCS) |
| `membership` | Membership recruitment and renewal campaigns |

The program type determines the AI brief generation strategy (copy tone, targeting approach, keywords, UTM structure) and feeds into the campaign naming convention.

### Google Ads Campaign Types

| Type | Description |
|------|-------------|
| `search` | Search (RSA, responsive search ads) |
| `demand-gen` | Display (YouTube, Discover, Gmail) |

### Campaign Goals

| Goal | Description |
|------|-------------|
| `event-registration` | Drive registrations for conferences and summits |
| `training-certification` | Drive enrollment for training courses and certification exams |
| `membership-growth` | Drive new membership sign-ups and renewals |

---

## Character Limits (Per Platform)

### Google Search (RSA)

| Element | Max Chars | Max Count |
|---------|-----------|-----------|
| Headline | 30 | 15 |
| Description | 90 | 4 |

### Google Display (Demand Gen)

| Element | Max Chars | Max Count |
|---------|-----------|-----------|
| Headline | 40 | 5 |
| Description | 90 | 5 |
| Business name | 25 | 1 |

### LinkedIn Sponsored Content

| Element | Max Chars |
|---------|-----------|
| Intro text | 600 |
| Headline | 200 |

### Meta Ads

| Element | Max Chars |
|---------|-----------|
| Primary text | 125 |
| Headline | 40 |
| Description | 30 |

### Reddit Promoted Posts

| Element | Max Chars |
|---------|-----------|
| Headline (post title) | 300 |
| Body (optional) | 500 |

### X/Twitter Promoted Tweets

| Element | Max Chars |
|---------|-----------|
| Tweet text | 280 |

---

## Campaign Naming Convention

Format: `Program | Base Name | Region | Objective | Targeting | Ad Format | Project | Funnel | Date`

Example: `Events | KubeCon NA 2025 | EMEA | Conversions | Intent | Search | CNCF | MoFU | 2025-06-01`

Used by the data pipeline to attribute campaigns to the correct foundation/project.

---

## Data Structures

### CampaignBriefRequest (brief generation input)

```
url: string                     — Event/course page URL
platforms?: CampaignPlatform[]  — ['google-ads', 'linkedin-ads', ...]
programType?: 'events' | 'education' | 'membership'
campaignGoal?: 'event-registration' | 'training-certification' | 'membership-growth'
targetAudience?: string         — User-provided audience description
valueProp?: string              — Key value propositions
totalBudget?: number            — Total campaign budget (USD)
refineFeedback?: string         — For refine endpoint
previousCopy?: object           — For refine endpoint
```

### CampaignCreateRequest (campaign creation input)

```
eventName: string
eventSlug: string
countryCode: string
registrationUrl: string
hsToken?: string                — HubSpot UTM token
campaignTypes: CampaignType[]   — ['search'], ['demand-gen'], or both
budgetUsd: number
searchBudgetPct: number         — 70 = 70%
startDate: string               — YYYY-MM-DD
endDate: string                 — YYYY-MM-DD
keywords: CampaignKeyword[]
headlines: string[]             — Search RSA headlines (15 max)
descriptions: string[]          — Search RSA descriptions (4 max)
displayHeadlines?: string[]
displayDescriptions?: string[]
displayBusinessName?: string
displayCallToAction?: string
geoTargets: string[]            — ISO country codes ['US', 'JP']
project?: string
driveFolderUrl?: string
platforms?: CampaignPlatform[]
linkedInConfig?: object         — LinkedIn-specific params
redditConfig?: object           — Reddit-specific params
metaConfig?: object             — Meta-specific params
twitterConfig?: object          — X/Twitter-specific params
```

### CampaignCreateResponse (campaign creation result)

```
success: boolean                — true if all platforms succeeded
campaigns: CampaignCreateResult[] — Results per platform/type
errors: string[]                — Platform-specific errors if any
```

### CampaignCreateResult (per-platform result)

```
platform: CampaignPlatform
type: CampaignType
campaignName: string
campaignId: string
adGroupCount: number
keywordCount: number
adCount: number
campaignUrl: string             — Direct URL to platform UI
steps: string[]                 — Step-by-step creation log
```

### CampaignMonitorResponse (monitoring result)

```
campaigns: CampaignMetrics[]    — Per-campaign metrics
accountTotals: AccountTotals    — Summed metrics across all campaigns
actionItems: ActionItem[]       — Pacing alerts and optimization suggestions
pulledAt: string                — ISO timestamp of data fetch
```

### Per-Campaign Metrics (shared across platforms)

```
campaignName: string
campaignId: string
status: string
impressions: number
clicks: number
ctr: number
spend: number
cpc: number
cpm: number
conversions: number
costPerConversion: number
dailyBudget: number
totalBudget: number
pacingPct: number
pacingLabel: string             — underspending | normal | constrained | overspending | severe
```

---

## SSE Event Types (Brief Generation)

| Event Type | Payload | Description |
|------------|---------|-------------|
| `status` | string | Progress message ("Scraping URL...", "Generating copy...") |
| `event` | object | Extracted event/course details |
| `hubspot_utm` | object | HubSpot UTM token found/created |
| `copy_token` | string | Token-by-token AI output (streaming) |
| `copy_done` | null | Copy generation complete |
| `copy_structured` | object | Parsed, validated ad copy JSON |
| `keywords` | array | Keyword list with match types |
| `linkedin_strategy` | object | LinkedIn targeting recommendation |
| `error` | string | Error message (may appear mid-stream) |
| `done` | null | Stream complete |
| `shutdown` | null | Server shutting down |

---

## Platform-Specific Gotchas

### Google Ads
- Budget is in micros (multiply by 1,000,000)
- No `campaign.start_date` / `campaign.end_date` in GAQL for API v23+
- Demand Gen campaigns use ad group level geo targeting (not campaign level)
- Duplicate campaign names cause creation failure; retry adds timestamp suffix
- RSA ads pin top 3 headlines for consistency

### LinkedIn Ads
- Images must be owned by org URN, not ad account
- `feedDistribution: NONE` required for dark posts (prevents company page visibility)
- Campaign groups must be ACTIVE status
- Budget as decimal string, not micros
- Timestamps in milliseconds
- Skills + Groups in one `or` block (separate AND blocks = too narrow)
- `callToAction` field not accepted; "Learn More" is automatic for article ads
- Idempotency: search by name across all statuses before creating
- Exclude employers: LF (`urn:li:company:33275771`) + CNCF (`urn:li:company:12893459`)

### Meta Ads
- ISO geo codes for targeting
- Objective-to-parameter mapping varies by campaign type

### Reddit Ads
- Token refresh with expiry buffer (tokens expire; must refresh before expiry)
- Subreddit targeting uses subreddit IDs, not names
- Account must be whitelisted in runtime config

### X/Twitter Ads
- OAuth 1.0a with HMAC-SHA1 signing (not OAuth 2.0)
- 1 request/second write rate limit
- Exponential backoff retry on 429 responses
- Only "lf-events" account currently supported

### HubSpot
- UTM lookup uses fuzzy name matching with scoring
- 15-second HTTP timeout per call
- If unavailable, falls back to campaign slug for UTM
