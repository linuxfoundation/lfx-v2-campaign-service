---
type: "Go Package"
title: "internal/platform/reddit"
description: "Reddit Ads API v3 client: OAuth2 token refresh, Campaign -> Ad Group -> Ad creation (can AUTHOR a promoted image post from an image URL, or promote a supplied post URL), campaign metrics reads built to Reddit's public OpenAPI spec (gated pending a live-account run)."
resource: "internal/platform/reddit"
tags:
  - platform-client
  - reddit
  - reddit-ads
  - oauth2
  - go-package
  - metrics
timestamp: "2026-08-05T00:00:00Z"
---

# internal/platform/reddit

Package reddit provides a Go client for the Reddit Ads API v3, porting the
upstream TypeScript `reddit-ads.service.ts` client. Credentials and account
configuration are injected via `NewClient`; the client never reads the process
environment.

Authentication uses OAuth 2.0 refresh-token exchange with a cached access token
and an expiry buffer (refresh shortly before the stated expiry). The token
endpoint and API base URL are overridable via functional options for `httptest`.

A resource-server **401 invalidates the cached access token**, on the STATUS LINE and before
the response body is read. Expiry alone is not enough: a revoked or rotated token keeps its
advertised `expires_in`, so the fast path would go on serving it long after the platform
started rejecting it. That only became reachable once the dispatcher began caching CLIENTS
across operations — a client rebuilt per operation started with an empty cache, so one
rejection cost one failure. Reading the body first would buy no accuracy (a 401 is a 401
whether or not its body arrives) and would hold the rejected token available to every
concurrent caller for the rest of the attempt timeout, so the unreadable and oversized arms
need no guard of their own.

Invalidation is **compare-and-clear**, not an unconditional clear: it takes the token the
rejected request actually presented and drops the cache only if it still holds that token.
With a shared client, request A can leave carrying `tok_1`, request B can refresh and cache
`tok_2`, and A's late 401 then names `tok_1` — clearing blindly would evict a token nothing
rejected and let a burst of late responses drive serial re-exchanges, defeating the
single-flight coalescing above. Matching makes it idempotent: the rejected token is dropped
exactly once, and every later 401 naming it is a no-op.

Matching the CACHE alone is not sufficient, because the cache is not the only place the
rejected value lives. `fetchToken` stores the token and unlocks; only under a LATER lock
acquisition does the leader publish it on the flight and retract `inflight`. In that window a
fast-path caller can take the token, be rejected, and clear the cache, while a caller that
missed the cache joins the still-published flight and is handed the very same token. So a
flight whose token MATCHES the rejected one is blanked and unpublished as well — as selective
as the cache clear, so a flight carrying any other token still survives. Blanking without
unpublishing recurses without bound (a waiter re-leads, finds the same poisoned flight, and
reads the blank again), and because invalidation can now retract a flight early, the leader's
own teardown compares identity before clearing so a stale leader cannot erase a newer flight.

`CreateCampaign` drives the Campaign -> Ad Group -> Promoted Post (Ad) hierarchy,
creating everything PAUSED with a lifetime budget and objective-aware bid params.
It normalizes geo targets once (trimmed, uppercased) so the ad-group label,
targeting, and region derive from a single source of truth, and computes the
start time up front so a same-day (past midnight-UTC) start is nudged to
now+buffer before the campaign POST. Post URLs are validated by parsing the URL
authority (`reddit.com`/`redd.it` and subdomains only) to prevent host spoofing,
and UTM parameters are merged into the URL query while preserving any fragment.

Supplied subreddit names (`r/golang` or `golang`) are sent to the ad-group
`communities` targeting field as NAMES with the `r/` prefix stripped (and
case-insensitive duplicates removed) -- Reddit's Ads API targets communities by
name, not `t5_` ID, and rejects `t5_` values as "invalid communities". This
matches the reference TS implementation, which sends the stripped names
directly.

TWO targeting dimensions can be rejected wholesale, and both are handled by one
retry. Communities are rejected when a subreddit name does not resolve;
`interests` are rejected because Reddit wants opaque ids (`technology_v3`) while
the brief generator produces human labels ("Machine Learning"), so in practice
every AI-generated brief sends values Reddit refuses. Either rejection returns a
400 that would otherwise ORPHAN the PAUSED campaign created moments earlier.

`baseTargeting` deliberately carries NEITHER dimension and is the retry payload;
`optionalTargeting` adds both for the first attempt. A 400 naming either one
therefore drops BOTH on a single retry rather than chaining two — retrying per
dimension sends a second doomed request, and every extra ad-group POST is another
chance to create one nothing points at. The Steps trail distinguishes what Reddit
REJECTED from what was proactively dropped alongside it, because an operator
re-adding targeting by hand needs to know which is which, and the two are re-added
in different places in Reddit Ads Manager.

This makes the failure SURVIVABLE, not the targeting correct: a campaign whose
interests and communities were dropped runs on keywords and geo alone. Resolving
labels to Reddit's ids is tracked separately (LFXV2-3261).

A conversion pixel is required by Reddit on EVERY campaign create, not only for
the `conversions` objective its documentation describes. It is read from the
CONNECTION (`conversion_pixel_id`, migration 000025) rather than per campaign,
because it identifies the advertiser: one pixel per ad account, the same for every
campaign created through it. A per-campaign value still overrides when present. A
create with none configured is refused BEFORE any upstream call, with a message
naming the connection — an operator reading it is looking at the campaign they
were editing, which is not where the fix lives.

Because create calls are mutating and paid, a FAILED create is classified by
whether the request may have reached Reddit. `isPreSendDialError` reports a Do
error as pre-send (request definitely NOT sent → clean not-created failure) ONLY
for proofs that no bytes left the client: DNS resolution failure, and
connection-refused/no-route/network-unreachable dial failures. NO TLS error is
treated as pre-send (matching the merged Meta client): a TLS error is not a
reliable pre-send proof for an arbitrary caller-supplied transport — a custom
transport can enable renegotiation, and a wrapping/retrying `RoundTripper` can
surface a `*tls.CertificateVerificationError` or `tls.RecordHeaderError` while
reading a response after forwarding the POST — so both flow to the UNCONFIRMED
path. Redirect following is still force-disabled on every client used, including
one supplied via `WithHTTPClient`: `NewClient` builds a fresh `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than mutating the caller's client. The rebuild
depends only on `http.Client`'s documented exported fields (layout-independent) and
never mutates the caller's client, so the override is unconditional — which keeps
3xx handling well-defined. Failures that
prove NEITHER pre-send NOR rejection are treated as UNCONFIRMED (may have been
applied): a 3xx on a MUTATING request (it reached a responder and may have
committed before redirecting — a 3xx on a GET is not a create), a
mid-flight/`Do`-time context error (the per-attempt timeout wraps the whole round
trip, so it can fire after the POST reached Reddit), and a read/decode failure on
a 2xx body are wrapped as `transportError`; a 5xx status is returned as an
`apiError` and classified by status. `createOutcomeAmbiguous` treats all of these
as "may exist", so callers require verification before a manual retry. A definite
4xx is NOT UNCONFIRMED — Reddit
received and REJECTED the request, so nothing was created and the caller gets a
clean failure.

## Authoring a promoted post (brief -> servable ad)

`CreateCampaign` can create the ad's creative itself, so a caller can go from a
brief to a servable ad without first hand-making a post in Reddit Ads Manager —
the parity path with the Google/Meta clients (which author their own creatives).
There are two ways an ad gets a creative, and an explicit post always wins:

- **`PostURL` supplied**: the given post is validated (`extractRedditPostID`) and
  promoted as-is. `ImageURL`/`CallToAction` are ignored.
- **`ImageURL` supplied, no `PostURL`**: the client AUTHORS an IMAGE post via
  `POST /profiles/{accountID}/posts`, then attaches its `t3_` id as the ad's
  `post_id`. Reddit's profile id IS the ad-account id (`t2_...`). Reddit marks
  this endpoint **legacy** (superseded by the structured-post job flow); not
  migrated, recorded so the next editor knows.

Reddit has NO LINK post type and will not auto-fetch an image from the
destination, so authoring requires an image URL; Reddit INGESTS that image at
post-create time and re-hosts it to `i.redd.it` (there is no separate
media-upload step). The post body is
`{"data":{"type":"IMAGE","headline":..,"allow_comments":false,"content":[{"media_url":..,"call_to_action":..,"destination_url":..}]}}`.

**`allow_comments:false` disables comments, and that is ALL Reddit documents it to
do.** An earlier version of this page said it marked the post promoted-only and
kept it out of feeds. That is two claims and both fail: the causal one is
**contradicted** (a separate PATCH exists purely to flip `allow_comments` on an
existing post — if it governed dark status, that PATCH would toggle a live ad
between hidden and public), and "the endpoint inherently creates promoted-only
posts" is **undocumented** (no visibility, publication or distribution field
exists on the create body or the GET that reads a post back). Reddit's "promoted
posts are only placed in feeds" wording belongs to the SELF-SERVE
promote-an-existing-post flow and does not transfer. Nothing in the client relies
on the post being invisible; see the ordering note below.

`call_to_action` must be one of Reddit's title-case labels (e.g.
"Learn More", "Sign Up", "Buy Tickets"); the client accepts a case-insensitive
value and resolves it to Reddit's exact label against `redditCTAs` (captured live
from the API; a caller typo fails locally with the accepted set named).
`destination_url` is the SAME UTM'd click URL the ad's `click_url` uses
(`buildRedditUTMURL`), so the post and ad agree on the landing URL. The headline
is the first ad variant's headline, else the event name, capped at
`maxRedditHeadlineRunes` (300).

The authored post's image URL, CTA, and headline are validated UP FRONT (before
any mutating call), so a bad value fails fast rather than after paid resources
exist. The post itself is created BEFORE the paid campaign (`Step 1.5`), and the
argument for that ordering is the **billing asymmetry**, not invisibility: an
unattached post carries no spend of its own while a campaign does, so a
post-authoring FAILURE degrades to a campaign + ad group with NO ad rather than
orphaning a PAID resource. Because a stray post's visibility is undocumented
rather than known-benign, it is treated as a stray to REMOVE: every arm where a
post may exist tells the operator to DELETE a post not attached to an ad, which is
correct whether or not the post is distributed.

A caller context cancellation during authoring is fatal, but only in conjunction
with ambiguity: `ctx.Err() != nil && createOutcomeAmbiguous(err)` returns a
name-carrying PARTIAL result so `Dispatch` RETAINS the claim (release keys on
`result == nil` alone, and the request layer wraps ctx errors as `transportError`
because a ctx error from `Do` can fire after the POST reached Reddit — so
`(nil, err)` there released the claim on a post that may exist and a retry
duplicated it). A non-ambiguous cancellation still aborts with `(nil, err)`.
Every NON-cancellation failure, including a plain 5xx, stays non-fatal and
degrades. The degrade step reports ONLY the HTTP status, never the error body
— a reflective Reddit validation error can echo the `destination_url` (which
carries the caller's permitted secret-bearing query params).

That degraded outcome is NOT the same as the missing-`PostURL` state, and must not
be recorded as one. With no `PostURL` and no `ImageURL` the caller never asked for
an ad, so `AdWarning` stays empty and the campaign is a clean `created`. When
`ImageURL` WAS supplied, an ad was requested and does not exist, so authoring sets
`AdWarning` (seeding the same field the ad-create path uses) and the dispatch
adapter persists `created_degraded`. Leaving it empty made a no-ad campaign
persist as a clean success, and because `created` is terminal the orchestrator's
idempotency check then refused to re-dispatch it — the campaign was permanently
ad-less while every status an operator could see said it had succeeded.

Authoring classifies its failure with the SAME three-way rule **and the same arm
order** as the ad-create path, so an operator is never told to take a manual action
that could duplicate a post Reddit already holds. `createOutcomeAmbiguous(err)` is
asked FIRST; `errors.As(err, &apiErr)` is used only to EXTRACT the status for the
message, never to select the arm. The order is load-bearing because the two are not
disjoint: `createOutcomeAmbiguous` returns true for an `*apiError` carrying a 5xx or
a mutating 3xx, so testing the type first swallows both into the "Reddit rejected
it" arm and tells the operator to author a post that may already exist.

**`Steps` is read as a RUNBOOK, so its arms must not contradict each other.** Three
defects on this path were one surface: (1) Step 4's generic ad guidance still fired
after an authoring failure — on the ambiguous arm it said "create the ads" while
`AdWarning` said the post may exist and to verify first, and its "No ad variants or
post URL provided" wording was false whenever `Variants` were supplied; (2) the
definite-failure arms omitted the campaign/ad-group context, so an operator
following them literally builds a SECOND campaign while the first sits PAUSED and
orphaned; (3) nothing said to delete a stray post. Step 4 now emits nothing when
`postWarning != ""` (guarded at the variant-LISTING level, which is the arm that
actually fired), and the ambiguous arms say DELETE.

**The campaign-state context is DEFERRED, not stated at Step 1.5.** Fixing (2) by
writing "campaign + ad group created (PAUSED)" into the Step 1.5 arms replaced a
missing-context defect with a premature-claim one — the same class, since Step 1.5
runs BEFORE the campaign POST (Step 2) and the ad-group POST (Step 3). The claim
became true only when both creates went on to succeed; on an ambiguous campaign
create the persisted runbook asserted both resources existed directly above
`"a PAUSED campaign may exist"`, and told the operator to attach an ad to a campaign
that may never have been created.

So the Step 1.5 arms confine themselves to the POST that actually happened
("continuing without an ad"), and a single step naming the real ids
(`Campaign <id> and ad group <id> were created (PAUSED)…`) plus an `AdWarning`
suffix are appended at **Step 4**, under `postWarning != ""`. Reaching Step 4 is
the proof the claim holds: every path between the campaign POST and it returns
early — ambiguous partial, malformed 2xx, definite failure, and the ad-group
equivalents — so control arrives only when both ids are decoded non-empty and both
resources are PAUSED. **A runbook sentence must be emitted from a point where its
claim is already known, not from where it is convenient to write.**

  - **UNCONFIRMED** — anything `createOutcomeAmbiguous` accepts: a `transportError`
    from an in-flight failure, a 2xx whose body carried no `data.id` (which
    `createPromotedPost` wraps as one precisely because the post may exist), or an
    `*apiError` with a 5xx / mutating 3xx. Verify BEFORE authoring again.
  - **FAILED** — an `*apiError` that is not ambiguous, i.e. a definite 4xx. Reddit
    received and rejected it, so no post exists and the operator can remediate
    directly.
  - **Pre-send** — the `default` arm (token refresh, body encode, request build, a
    refused/unresolvable dial). A new non-`apiError` shape must be checked against
    `createOutcomeAmbiguous` before it lands here.

The dispatch adapter SANITIZES
both `PostURL` and `ImageURL` (query/fragment stripped) before snapshotting the
config, since a signed image URL can carry a secret and `config_snapshot` is
stored unencrypted.

## Campaign status toggle

`UpdateCampaignAndChildrenStatus(ctx, campaignID, adGroupID, adID, status)` pauses/resumes a
campaign AND cascades to its child ad group + ad, because the create path PAUSES all three
entities — toggling only the campaign to ACTIVE would leave the ad group/ad PAUSED and the
campaign would not serve. Each entity is set via `PATCH /ad_accounts/{accountID}/{entity}/{id}`
with `{"data":{"configured_status": "ACTIVE"|"PAUSED"}}` — the same envelope + `configured_status`
field the create path sets (`configured_status` is the advertiser-set state, distinct from the
read-only `effective_status`). The cascade ordering is STATUS-DEPENDENT so a partial failure
never leaves paid delivery running unattended: on ACTIVATE it lifts the children first (ad, then
ad group) while the campaign gate is still PAUSED and gates them, then flips the CAMPAIGN gate
LAST — a child failure before the gate flip leaves nothing serving; on PAUSE it flips the CAMPAIGN
gate FIRST (delivery stops immediately), then the children. Two edge cases:
ACTIVATING requires the FULL servable tree: it is REFUSED before any PATCH when EITHER the ad
group id OR the ad id is missing (a reddit create can land a campaign + ad group but no ad — the
no-`PostURL` path returns AdCount 0 / empty AdID — yet still persist as "created"; activating it
would leave nothing to serve, so the caller must not persist "active"). The refusal returns
`domain.ErrCampaignNotProvisioned` (→ 409, a client/state error; the platform is never called).
PAUSING with no child id is fine (pausing the parent already halts delivery) and toggles the
campaign alone. On PAUSE (campaign-gate-first), if the campaign PATCH commits but a later child
PATCH fails, the result is a `partialCascadeError` whose `Unconfirmed()` is true (via
`IsOutcomeUnconfirmed`), so the service reports 503-unconfirmed ("verify before retry") rather than
"not modified" — a retry re-runs the idempotent cascade. (On ACTIVATE, children-first, a child
failure occurs BEFORE the campaign gate opens, so nothing is serving and it is a plain error, not
a `partialCascadeError` — `partialCascadeError` is PAUSE-only.) `StatusActive`/`StatusPaused`
are the two accepted values. Every id is validated with the letters/digits/underscores guard
(rejecting `/`, `?`, `#`) before interpolation. A PATCH is idempotent, so `request()` may
safely retry it on a 429. (`UpdateCampaignStatus(ctx, campaignID, status)` toggles the campaign
alone and is retained as the per-entity building block / for callers with only a campaign id.)
The child ids are read from the persisted `CampaignResult` (`adGroupId`/`adId`) by the reddit
dispatcher's `ToggleStatus`, which now receives the full persisted `*model.Campaign`.

`CampaignResult` also carries `accountId` (LFXV2-3050), stamped from `c.account.AccountID` at
every construction site including the partial-result paths, so the dispatcher's provenance guard
can refuse a toggle or metrics read whose connection has since been re-pointed to another ad
account. Reddit has NO recoverable fallback for it: `redditUrl` is the bare
`https://ads.reddit.com` constant and never carried an account, so a row written before this
field existed records no provenance at all and is treated as "unknown, proceed". See
`internal-dispatch.md` for the guard itself.

## Metrics reads — contract from Reddit's public OpenAPI spec (LFXV2-3282)

`GetCampaignMetrics(ctx, campaignID, window)` reads impressions, clicks, and spend for a
campaign. **It is unreachable in a default deployment**: the dispatcher above it
(`RedditDispatcher.ReadMetrics`) is gated on `REDDIT_METRICS_ENABLED == "true"` and otherwise
returns `domain.ErrMetricsUnsupported`.

The request and response shapes come from Reddit's **official public OpenAPI document**
(`https://ads-api.reddit.com/api/v3/openapi.json`, linked as "Download Specs" from
`https://ads-api.reddit.com/docs/v3/`), operation `POST /ad_accounts/{ad_account_id}/reports`,
schemas `Report` and `ReportMetric`. Reddit's own introduction states the Ads API "is open to
all developers and does not require allowlisting or approval from Reddit to access".

**This supersedes the LFXV2-2995 BLOCKED finding**, which recorded that Reddit published no
public documentation for v3 reporting and that the shape here was a guess inferred from this
client's own conventions. That finding is no longer accurate. Reading the spec falsified five
of the six things previously guessed:

| Element | Previous guess | Spec |
| --- | --- | --- |
| Path + method | `POST /ad_accounts/{id}/reports` | correct — the one guess that held |
| Campaign scoping | `campaign_ids` array | `filter` string DSL, `campaign:id==<id>` |
| Field names | lowercase `impressions`, `clicks`, `spend` | UPPERCASE enums `IMPRESSIONS`, `CLICKS`, `SPEND`, `CAMPAIGN_ID` |
| Response `data` | bare array of rows | object with a `metrics` array (plus `pagination`) |
| `spend` | decimal-currency string, scaled by 1e6 | `int64` already in **microcurrency** |
| `starts_at`/`ends_at` | bare `YYYY-MM-DD` | `YYYY-MM-DDTHH:00:00Z`, hourly granularity only |

The request schema sets `additionalProperties: false`, so the previous `campaign_ids` key
would have been rejected outright. `spend` being microcurrency (not a decimal string) is the
correction with the largest blast radius: the old code multiplied by 1e6, which against the
real contract would have reported every spend figure one million times too large had it
decoded at all. Note "microcurrency" means the **ad account's own billing currency**, which
this client does not read — `CostMicros` is therefore micros of an unspecified currency, as it
is for X, and must not be summed across platforms.

`CAMPAIGN_ID` is requested as a **field**, not merely a breakdown, so every row carries the id
the provenance check verifies against. `breakdowns` is omitted so Reddit aggregates over the
whole window; the accumulation loop still sums correctly if that is wrong. `time_zone_id` is
omitted because the spec documents it as defaulting to UTC, which is the zone
`dateRangeForWindow` renders in.

**What remains unverified**: no request has been made against a live Reddit ad account (this
repository holds no Reddit credentials), which is why the gate stays on. A schema cannot
express whether a campaign with no activity yields an empty `metrics` array or a row of
explicit zeros, nor whether `ends_at` is inclusive of its final hour. Both readings are handled
without a wrong answer, and both are recorded at their site rather than assumed away.

### Refusing rather than reporting a plausible number

Every metric field is decoded as a **pointer**, and a missing or null field is an error. The
spec types `impressions`, `clicks`, and `spend` as `["integer","null"]`, so Reddit may send an
explicit null; a value field would decode that to `0`, which is indistinguishable from a
campaign that genuinely served nothing. This is stricter than the googleads client, which
treats an empty metric as a real zero — that is correct there because Google Ads is
*documented* to omit zero-valued metrics, and Reddit's spec documents no such behaviour.

The refusal names which requested fields were absent, so a wrong assumption is diagnosable in
one read rather than by debugging a decoder. No upstream value, id, or account id is echoed
into an error; errors report the bare `reports` path because the account id is interpolated
into the real one.

Response handling distinguishes:
- An **explicit empty `metrics` array** is real "no activity" (zero metrics), not an error.
- A **null or missing `data`**, a `data` that is not an object, or a **null/missing `metrics`
  array** are all decode errors. Consumers must not assume missing data means zero activity.

Rows are validated before accumulation: the row's `campaign_id` must match the requested
campaign (the `filter` is not trusted to have scoped the report), counters must be
non-negative, and a row reporting clicks with zero impressions is rejected as impossible —
the same guard the LinkedIn client applies. Checked additions stop a running total from
wrapping past `MaxInt64`. CTR is derived from the **totals**, not read from the row's own
`ctr` field nor averaged per row.

`dateRangeForWindow` maps the shared `model.MetricsWindow` literal
(`today`/`last_7_days`/`last_30_days`/`this_month`/`last_month`) to the required hourly
timestamp pair, anchored to the UTC calendar date and handling the last-month-at-month-end
boundary from a first-of-month anchor (never `AddDate(0,-1,0)`, which normalizes an invalid
day into the following month). The end bound is the `23:00` hour of its day, since midnight
would ask for a range stopping as the final day begins. `ValidateMetricsWindow` is
package-level and clock-free so a caller can reject a window without credentials, and it is
checked before the account is resolved so a permanent 400 is not masked as a retryable
connection failure. `ErrInvalidCampaignID`/`ErrUnsupportedWindow` are typed sentinels
(`errors.Is`-discriminable).

The campaign-id charset guard now protects two interpolation sites, not one: the URL path and
the `filter` DSL, where a comma would split one filter term into two and silently widen the
report's scope.

The `model.MetricsWindow`/`model.CampaignMetrics` types and the `service.MetricsReader`
interface this depends on come from the platform-agnostic metrics foundation
(LFXV2-2997, merged to `main`), not from a per-branch copy.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` reddit adapter (see [internal/dispatch](internal-dispatch.md))
interprets OAuth2 (clientId/secret/refreshToken) credentials; AccountID comes from the
connection.

It implements `StatusToggler`: `resolveRedditClient` (shared with `Dispatch`, so a create
and a toggle accept exactly the same connections) builds the client, then
`client.UpdateCampaignAndChildrenStatus` PATCHes `configured_status` on the campaign AND its
child ad group + ad (read from the persisted `CampaignResult`) — because the create path
PAUSES all three, so toggling only the campaign would not serve.

See [internal/platform/reddit](../../../internal/platform/reddit).
