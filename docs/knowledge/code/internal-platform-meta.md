---
type: "Go Package"
title: "internal/platform/meta"
description: "Meta (Facebook/Instagram) Ads Graph API client: Campaign -> Ad Set -> Ad creation with objective mapping and geo/budget validation, optional single-image ad creatives attached EITHER by URL as `link_data.picture` OR by stored bytes uploaded to `/adimages` as `link_data.image_hash` (mutually exclusive per creative, both supported), campaign status toggle cascade over ad set and ads, live campaign metrics reads, and ad-account discovery — a paginated `/me/adaccounts` walk that asks about the TOKEN rather than any one account, returns known-bad accounts with their reason instead of filtering them, and fails rather than truncating when the walk cannot be completed."
resource: "internal/platform/meta"
tags:
  - platform-client
  - meta
  - facebook-ads
  - graph-api
  - go-package
timestamp: "2026-07-13T19:22:00Z"
---

# internal/platform/meta

Package meta provides a Go client for the Meta (Facebook/Instagram) Ads Graph
API, ported from the upstream TypeScript `meta-ads.service.ts` client.
Credentials and account configuration are injected via `NewClient`; the client
never reads the process environment and uses only the standard library.

Authentication is a Graph API Bearer access token. `CreateCampaign` drives the
Campaign -> Ad Set -> Ad(s) hierarchy, creating everything PAUSED, with
objective->parameter mapping (awareness/traffic/engagement/leads/conversions),
placement/promoted-object building, and UTM URL construction that preserves any
URL fragment.

The `leads` objective INTENTIONALLY DIVERGES from the `@lfx-one/shared` TS
contract (`campaign.constants.ts` maps leads -> LEAD_GENERATION with a page_id
promoted object). LEAD_GENERATION optimization requires the ad creative to carry
an on-Facebook instant lead form (`lead_gen_form_id`), which this client does not
construct — it only builds a website-click creative pointing at the registration
URL. Adopting LEAD_GENERATION would fail at ad-set/ad creation, after the paid
campaign already exists. To stay fail-safe, `leads` runs an interim WEBSITE-TRAFFIC
campaign — OUTCOME_TRAFFIC optimizing for LINK_CLICKS to the registration
(lead-capture) URL, with no promoted object. OUTCOME_TRAFFIC is used (not
OUTCOME_LEADS) because OUTCOME_LEADS + LINK_CLICKS requires a `pixel_id` +
`custom_event_type` that this interim flow does not supply — that pairing would
create the campaign then fail at the ad set, orphaning it. OUTCOME_TRAFFIC
supports LINK_CLICKS with no pixel requirement, so the flow is spendable
end-to-end. Full LEAD_GENERATION / instant-form (or OUTCOME_LEADS + pixel) parity
with the TS contract is deferred (LFXV2-2665).

Ad creatives are website-click ads built from `object_story_spec.link_data`
(page id, the UTM click URL, primary text, headline, optional description). A
variant may additionally carry an image, which turns the ad into a SINGLE-IMAGE
ad. There are TWO ways to supply one and they COEXIST, because Meta documents two
different creative fields for them:

- `AdVariant.ImageURL` — attached to that variant's creative as
  `link_data.picture`. No upload; Meta fetches the URL server-side.
- `AdVariant.ImageAssetID` — a creative asset previously uploaded against the
  brief. The dispatcher resolves it to `ImageBytes`, the client POSTs those bytes
  to `/act_<id>/adimages`, and the returned account-scoped hash is attached as
  `link_data.image_hash`.

Both are additive — a variant with neither produces exactly the previous bare-link
creative. They are MUTUALLY EXCLUSIVE per variant (see below), which is a
restriction on one creative, not on the codebase: each variant independently
chooses a route, and a campaign may mix by-URL and by-bytes variants.

`picture` is the DOCUMENTED by-URL field on `AdCreativeLinkData`: "URL of a
picture to use in the post. Specify this field or `image_hash` but not both. ...
The image specified at the URL will be saved into the ad accounts image library."
META fetches the image server-side, so the client never dereferences the caller's
URL and acquires no outbound-fetch (or SSRF) surface, and the whole creative stays
one JSON create inside the ordinary hardened request path — no second transport.
Because the reference makes `picture` and `image_hash` mutually exclusive, a
single creative carries exactly one of them. `validateVariantImage` REFUSES a
variant supplying both, locally and before any upstream call, rather than letting
Meta reject it after the campaign and ad set already exist. `objectStorySpec`
then encodes the same exclusivity structurally: `picture` is set only when no hash
was uploaded.

`image_hash` comes from `POST /act_<id>/adimages`, with the image sent as a
multipart FILE part.

ON THE PART'S FIELD NAME — the docs do not settle it, and an earlier version of
this entry claimed they did. The reference documents exactly two CREATE
parameters, `bytes` ("Image file", typed "Base64 UTF-8 string") and `copy_from`,
and documents NO multipart file field: it is SILENT on multipart upload
altogether. So "the parameter list says `bytes`, therefore the part must be named
`bytes`" does not follow — that list describes a different (base64-in-a-scalar)
way of sending the image.

What shows the name is not load-bearing is that Meta's two OFFICIAL SDKs send
DIFFERENT names against this same endpoint, both in production: the Python SDK's
`FacebookRequest.add_file` builds `file_key = 'source' + str(self._file_counter)`
and uploads under `source0`, while the PHP SDK's `AdImage.php` sets
`AdImageFields::FILENAME` and uploads under `filename`. Two vendor SDKs, two
names, one endpoint ⇒ the upload handler accepts any file part. The client's
current name is therefore not a defect and is left alone.

What IS load-bearing is that the part carries a FILENAME: that is what makes
Graph treat it as a file upload rather than a scalar field, and Meta echoes the
filename's BASENAME back as the key of the `images` map (the PHP SDK reads
`images[basename($filename)]`). Whether that filename needs a real EXTENSION is
undocumented in both directions — no authoritative source states a rule, and Meta
sniffs content for format — so nothing in the code or tests asserts one.

The response is `Map { string: Map { string: Struct { hash, url, ... } } }` under
`images`. Because the key's contract lives in SDK source rather than in the
reference, `uploadImage` does not derive it: it ENFORCES that exactly one entry is
present (one upload yields one entry) and reads that entry BY VALUE. The count
guard is not decoration. The earlier revision iterated the map and returned the
first non-empty hash, which under Go's RANDOMIZED map iteration returned an
arbitrary hash from a multi-entry response — and that hash becomes
`link_data.image_hash` on a creative that spends money, so the failure mode was
the wrong creative on a live paid ad, silently. Anything other than exactly one
entry, or an entry whose hash is empty, is now refused as a `transportError`.

CORRECTING AN EARLIER OVER-GENERALISATION: an earlier revision called the same
edge with a `url` field, which is NOT an accepted input (`url` is a field on the
RETURNED image object), so it would have been rejected against a live account
after the campaign and ad set already existed. That verdict is about the `url`
PARAMETER and was recorded as if it disqualified the ENDPOINT — the note read
"nothing in the repo calls `/adimages` any more", which is no longer true and was
never the necessary conclusion. `/adimages` is the only route that attaches an
image the service holds as STORED BYTES rather than as a URL, so it is called
again — this time on the multipart transport the official SDKs use.

Be precise about which transport, because the reference and the code do not
match and the mismatch reads as a bug: Meta's `/adimages` reference documents
two create parameters, `bytes` (a Base64 UTF-8 SCALAR) and `copy_from`, and
documents no multipart file field at all. `uploadImage` does not use the `bytes`
parameter — it sends a raw multipart file part named `source`. That is not the
documented parameter and is not claimed to be; the published docs are SILENT on
the multipart mechanism, and the evidence for it is that Meta's own SDKs ship it
(Python uploads under `source0`, PHP under a filename param). See
`docs/knowledge/log/2026-08-21-LFXV2-3295-adimages-part-name-and-one-entry-guard.md`
for the full derivation — including why "the parameter list says `bytes`, so the
part must be named `bytes`" does not follow.

The URL is validated up front alongside the copy-limit checks (absolute, https,
no embedded userinfo) so a malformed URL is rejected BEFORE any paid resource
exists, rather than at the per-variant creative step where the campaign and ad
set are already created. Meta fetches the URL, so userinfo is rejected
specifically to avoid handing a basic-auth secret to Meta. The both-supplied
refusal runs in the same pre-spend pass.

Neither path logs caller bytes, a byte count, or a checksum: an upload failure
names only which variant failed, so the by-bytes path needs no analogue of
`scrubURLFromErr` — there is no caller-supplied value in its error text to scrub.

A creative rejected over its picture URL is non-fatal per-variant and is reported
in `Steps` like any other per-variant failure. Because the URL now travels as a
creative parameter, Meta can echo it back in `error.message`, which `do` copies
verbatim into `APIError.Message`; every per-variant failure step therefore renders
through `scrubURLFromErr`, which replaces the caller's URL (verbatim or
percent-encoded) with its `redactURL` form before the message reaches the
persisted `Steps` sink. A caller URL may be pre-signed — the signature is a
bearer credential — and `Steps` are persisted and logged, so the step keeps the
identifying scheme+host+path and drops the query, fragment, and userinfo. This
mirrors `displayMetaUTMURL`: the full value still goes to Meta, only the persisted
copy is sanitized.

`scrubURLFromErr` FAILS CLOSED, and it does so STRUCTURALLY rather than by
searching the text for the secret. When the image URL carries a query or fragment
— the components that hold a pre-signed signature — upstream-derived text is never
emitted at all: the step becomes the URL's `redactURL` form plus a fixed
"message withheld" note. When the URL has no query or fragment there is nothing
secret to protect, so the message is kept with the URL replaced in place.

The structural rule replaced an earlier verify-the-text approach, which was not
sound. The text reaching this sink has been through transformations that
replacement cannot invert and a verifier cannot enumerate: `do` truncates a
non-Graph body at 300 runes (clipping a signature mid-value), and a proxy or WAF
may re-encode or line-wrap the echo. A substring verifier only rejects the residues
it thought to look for — an echo of `?sig=SECRET_SIG` wrapped to `?sig=SEC\nRET_SIG`
defeats both the replacement and a prefix scan, because no contiguous run of the
value survives to be found. Arbitrary transformed text cannot be proven clean by
substring checks, so the scrubber no longer tries; it withholds on the input's
shape. An unparseable URL is likewise treated as secret-bearing. Every return path,
including the withheld one, is clamped to the caller's `max`, since `redactURL`
preserves the caller-controlled path.

Inputs are validated up front, before any mutating call: geo targets are checked
against ISO 3166-1 alpha-2 and comprehensively-sanctioned countries are
excluded; per-variant copy is rejected up front when it exceeds Meta's limits
(primary text 125, headline 40, description 30 characters, counted by rune) so
over-limit copy fails before any paid campaign/ad-set exists rather than at
non-fatal creative creation; `CampaignInput.Budget` is denominated in the ad
ACCOUNT's own currency — the client does NO foreign-exchange conversion, so the
caller must pass an amount already in that currency — and it is bounded
(rejecting rounds-to-zero and overflow-scale values) then converted to minor
units by multiplying by the account's minor-unit offset
(`AccountConfig.CurrencyOffset`) rather than a hardcoded ×100. That offset is
DERIVED from the account's ISO 4217 currency code, not fetched: the Meta AdAccount
node exposes only `currency` (the ISO code) — it does NOT expose a
`currency_offset` field (only the separate Currency node does). CreateCampaign
maps the code through an AUTHORITATIVE supported-currency table
(`currencyMinorUnitOffset`), which is the single source of truth: the zero-decimal
currencies (JPY, KRW, CLP, VND, and the rest of the standard set) map to 1, and the
enumerated two-decimal currencies (USD, EUR, GBP, and the other supported majors)
map to 100. There is NO fall-through default — a code absent from the table
(blank, or a well-formed-but-unknown code such as `ZZZ`) is treated as unsupported.
The offset is never guessed: when `AccountConfig.CurrencyOffset` is unset (zero) — the normal
case for a dispatch built from a persisted connection, which carries only
account/page/app IDs — CreateCampaign fetches the account's `currency` (ISO code)
from the ad-account object during the account preflight, BEFORE any mutating call,
derives the offset from it, and fails closed if the currency is unknown or absent.
Silently defaulting to 100 would encode a zero-decimal-currency
(JPY/KRW/CLP) budget 100× too high, and a warning after resource creation cannot
prevent that budget from being activated. The account currency is
authoritative: a caller MAY set a positive `CurrencyOffset` as a FALLBACK, but if
the preflight returns a recognized currency whose true offset differs, the request
is REJECTED (a stale override would mis-scale the budget). The explicit offset is
only used when the preflight fails or its currency isn't in the supported map. The
preflight GET always runs (it also verifies access). A negative offset is
rejected as malformed. The preflight also reads `account_status`: a
successful GET is not treated as "active" — if the account is in a known-inactive
state (disabled, closed, pending review/settlement, etc.) CreateCampaign fails
BEFORE any mutating call rather than creating a paid campaign Meta would reject
later; an unreported status (0) or any value not known to be bad is allowed
through. `CampaignInput.Project` is
also required (rejected up front if empty/whitespace): the campaign name's
Project segment must be the caller-supplied canonical LFX project slug, so the
client never silently substitutes a placeholder that could mis-attribute a
campaign to the wrong project.
Dates are parsed strictly (impossible calendar dates rejected) and a
past start date is refused, with a same-day ad-set `start_time` nudged to
now+buffer. `doRequest` retries HTTP 429 and Graph rate-limit envelope codes
(4/17/32/341/613/80004) with bounded backoff, draining the body before close, and a
truncated response body is surfaced rather than reported as a false success.
**Creates go through `doCreate`, which suppresses that throttle retry.** The retry
and `createOutcomeAmbiguous` would otherwise hold opposite premises about the same
response: the classifier calls a throttle UNCONFIRMED precisely because Meta may have
committed the node before reporting it, while the retry loop would re-POST the create
on that same signal — producing two campaigns (or ad sets, or ads) with one name
inside a single call, which the start-of-flow name lookup cannot see or reconcile. A
throttled create therefore returns immediately and is classified UNCONFIRMED. What
happens to whatever Meta committed is then an OPERATOR question, not an automatic one:
`findCampaignByName`/`findAdSetByName` is the mechanism that could adopt it, but that
lookup is opt-in and off at every call site today (see "Campaign and ad-set idempotency
by name" below), and nothing re-dispatches a retained partial — so the UNCONFIRMED result
is surfaced for verification in Ads Manager rather than reconciled on a next run. The
suppression is scoped to creates, not to POST as a method — the status-update POSTs
assert a desired state, so repeating them changes nothing and they keep the retry.
Redirect following is force-disabled (a shared `noFollow` `CheckRedirect` policy).
For a `WithHTTPClient`-supplied client, `NewClient` builds a FRESH `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than value-copying the caller's client (an
`http.Client` must not be copied after first use — a copy duplicates its internal
mutex while sharing the request-cancellation map). So a 3xx on a mutating POST is
surfaced rather than followed to a different target; `createOutcomeAmbiguous`
classifies a mutating 3xx (gated on the method) and any 5xx/transport failure as
UNCONFIRMED, so a create that may have committed is not blind-retried into a
duplicate. The status is preserved even when the response body is unreadable or
oversized, so an ambiguous outcome is never downgraded to a definite failure.

## Campaign and ad-set idempotency by name

Unlike Google Ads, Microsoft, and LinkedIn — which reject campaign name duplicates
and let a retry discover the existing id via a name query — Meta's Graph API does
NOT enforce campaign-name uniqueness and exposes NO create-idempotency key. A
blind retry would silently create a second paid campaign with the identical name.
To close this window, `CreateCampaign` can run a "poor-man's idempotency"
reconciliation: before POSTing to create a campaign or ad set,
`findCampaignByName` / `findAdSetByName` (using Graph API
`filtering=[{"field":"name","operator":"EQUAL","value":name}]` lookups) check
whether the name already exists. If found, the existing id is reused and the
resource is not re-created.

The lookup is OPT-IN, gated on `CampaignInput.ReconcileByName`, which is false at
every call site today. An unconditional lookup cannot help the case it exists for
and can only harm the cases it does reach. It cannot help because the orchestrator
never re-dispatches a retained partial — it answers "reconciliation required"
(`internal/service/orchestrator.go`) — so no retry arrives here to be reconciled.
It harms because the campaign name is not brief-unique (below), so a different
brief sharing the four name segments would be silently adopted; and because DELETE
frees the (brief, platform) slot LOCALLY without touching the ad platform
(`docs/api-catalog.md`), so the documented delete → re-dispatch flow — the supported
way to correct a campaign created with the wrong budget — would find the old
campaign by name (budget is not a name segment), reuse it and its ad set, and report
success while re-running the old budget. When LFXV2-2665's reconcile path lands it
is the caller, which knows it is resuming a specific dispatch generation, that sets
the flag; the client cannot infer it.

When the lookup does run and FAILS, for any reason, an UNCONFIRMED partial result is
returned (the resource MAY exist, verify before retrying). The line is not
transport-vs-4xx and it is not pre-send-vs-sent: a 2xx with no `data` field, a
`paging.next` with no cursor, exceeding the page cap with pagination still pending, a
single match whose id is empty or non-numeric, context cancellation mid-enumeration, a
definite 4xx and a pre-send dial error all fail closed the same way. The lookup exists to
establish that a NAME IS ABSENT so a prior ambiguous attempt can be adopted instead of
duplicated, and a request that never left the process establishes absence no better than a
timeout does. (`createOutcomeAmbiguous` asks a different question — *could this request
have created something* — which is the right question for a POST and the wrong one for a
GET, where it collapses to "was the transport ambiguous".)

The one clean error is a definite CONFLICT, marked with `errLookupConflict`: the lookup
succeeded, enumerated the name and read a match that is not `PAUSED`, or whose objective
differs from the requested one. Absence is not unconfirmed there — presence is confirmed,
with a stated reason. Nothing was created, nothing can be adopted, and a retry re-reads the
same stable conflict rather than POSTing a duplicate, so no partial is retained and no one
is sent to Ads Manager to verify what the error already says. The ad-set lookup splits the
same two ways.

Both names are deterministic, but they are composed differently and carry different
uniqueness guarantees. The CAMPAIGN name is the attribution name (`Events | event |
region | objective | Intent | Social | project | MoFU`) — deterministic for a given
brief, but NOT brief-unique: two briefs sharing event name, region, objective and
project produce the same name. The AD-SET name is only `"<EventName> - <objective
label>"`; it is disambiguated by the campaign it is queried under, not by the name.
Because a name match alone therefore cannot prove a campaign belongs to this brief,
`findCampaignByName` fails CLOSED on anything it cannot pin down: more than one match,
a non-numeric id, a status other than `PAUSED`, or an objective other than the
requested one all abort rather than reuse. Only a single PAUSED campaign with a
matching objective is reused. The ad-set lookup runs only when the campaign was in
fact reused — a campaign created moments ago in the same call cannot already have a
child ad set, so the extra GET would add a failure point with nothing to find.

Both ids the create path produces are gated with `numericIDRE` before they are used
or published. The lookups already gate the ids they return, and every consumer of a
STORED id gates it again, which leaves a FRESHLY CREATED id as the only one that
would otherwise reach `CampaignResult` — and from there durable storage and
`"/{id}/..."` paths on later calls — unvalidated. A 2xx carrying `"123?fields=x"` or
a padded `"123 "` would be persisted now and rejected much later, at a call site with
no idea a campaign was created. Both gates classify the outcome as a malformed
SUCCESS rather than a failure: the resource almost certainly exists (Meta answered
2xx), it simply is not addressable by id, so a name-carrying partial is returned and
the dispatcher keeps the claim instead of freeing it for a duplicating retry.

This builds the client-layer half of LFXV2-2665 for campaign- and ad-set-level
resources. Reaching it on a production retry needs the other half: the orchestrator
re-invoking the dispatcher on a RETAINED claim (today `ClaimCampaignDispatch` hits
`ON CONFLICT DO NOTHING` and the orchestrator returns "reconciliation required"
without calling the dispatcher) AND that caller setting `ReconcileByName`. Until
both land the lookup is exercised only by tests, which is the intended state — the
capability is in place and deliberately unreached rather than on by default.

`CampaignResult` carries `AccountID` (LFXV2-3050), stamped at all six construction sites
including the partial-result paths, so the dispatcher's provenance guard can refuse a toggle or
metrics read whose connection has since been re-pointed. It is stored VERBATIM as the connection
carries it — the `act_<digits>` form comes from `design/connection.go`'s `^act_[0-9]+$`
constraint, not from this field — and the field is UNTAGGED like the rest of this struct, so the
persisted key is the Go field name. Rows written before it existed stay checkable via the `act=`
parameter of `metaUrl`, which carries the digits with the `act_` prefix STRIPPED; the dispatcher
normalises both sides. See `internal-dispatch.md` for the guard itself.

## Campaign status toggle

`UpdateCampaignAndChildrenStatus(ctx, campaignID, adSetID, status)` pauses/resumes a campaign
AND cascades to its ad set + ads, because Meta's create PAUSES all three — toggling only the
campaign to ACTIVE would leave the ad set/ads PAUSED and the campaign would not serve. Each
entity is updated by POSTing to its node id with `{"status": "ACTIVE"|"PAUSED"}` (Meta's Graph
API updates a node by POSTing to the node id with the changed field; the same status enum the
create path sets). Meta persists the ad set id (in the campaign result) but NOT the individual
ad ids, so the ads are DISCOVERED via `GET /{adSetID}/ads` (paged; an unexpected/looping
cursor, the page cap, or an ad with no usable id fails the discovery rather than silently
truncating). Ordering is STATUS-DEPENDENT (Meta gates a child's serving by its parent's status
— a paused parent is inherited by all children) and DISCOVER-FIRST: the ads are enumerated (a
GET) BEFORE any mutation, then on ACTIVATE the ads are updated, then the ad set, and the campaign
is flipped ACTIVE LAST (every child stays gated by the paused campaign until that flip, so a
mid-cascade failure leaves NOTHING serving); PAUSE flips the campaign gate FIRST then the
children. Ids are validated numeric up front (nothing applied on a bad id); activating with no
ad set id — or with an ad set that has ZERO ads (a degraded broker campaign, since Meta treats
per-variant ad failures as non-fatal at creation) — is refused BEFORE any mutation via
`ErrCampaignNotServable` (the dispatcher maps it to a deterministic 409, so a degraded campaign
converges to a "reprovision" error instead of looping on a transient 503 that re-POSTs the ad
set each retry). A failure once an upstream change may have landed — the pause path, an ambiguous
5xx/transport outcome, OR any activate-path failure AFTER the first ad (or the ad set) was
updated — is a `partialCascadeError` (Unconfirmed). A DEFINITE (4xx) failure BEFORE any child has
been mutated (including a discovery failure, which now precedes all mutation) is a clean failure. `StatusActive`/`StatusPaused` are the
accepted values; ids are validated numeric (`numericIDRE`) before interpolation. The narrower
`UpdateCampaignStatus(ctx, campaignID, status)` (campaign node only) is retained as the
building block. `IsOutcomeUnconfirmed(err)` exposes the shared ambiguity classifier (and honors
the `Unconfirmed()` behavioral interface) so a caller can tell a maybe-applied outcome
(transport/5xx/3xx-mutating, or a partial cascade) from a definite rejection.

## Metrics reads

`GetCampaignMetrics` (in `metrics.go`) reads live impressions, clicks, spend (cost), and
CTR for one campaign over a predefined date-range window (e.g. `LAST_30_DAYS`) via a single
Graph API `GET /{campaignID}/insights` read with a `date_preset` parameter. Both interpolated
values are validated BEFORE they reach the request string, but by DIFFERENT means: the window
is checked against an allow-list of supported presets (`supportedMetricsWindows`), since Meta's
insights endpoint has a fixed preset vocabulary, while the campaign id is checked against a
numeric pattern (`numericIDRE`) — there is no enumerable set of valid ids to allow-list.
Numeric metric fields arrive as JSON strings. `impressions` and `clicks` (integers) are
parsed via `parseMetricInt`, which treats empty strings (Meta omits zero-valued optional
fields) as zeros rather than parse errors, and rejects both unparseable and negative values.
`spend` (decimal) is parsed separately via `strconv.ParseFloat`, then scaled to micros (cost
in whole units → ×1,000,000) and rounded (not truncated) to `CostMicros`, guarding against
non-finite (`NaN`/`Inf`) results.

No malformed-value error echoes the offending bytes. `parseMetricInt` and the spend guards
report the field name, the reason, and the value's byte LENGTH only, because these errors reach
`BriefService.GetCampaignMetrics`'s default branch and are logged — and `safeErrSummary` bounds
and normalises text without knowing whether the text is ours, so a short printable secret
sitting in an upstream field would pass through it intact. The tests that pin this pick
their inputs to REACH each guard: a value like `-1SECRETMARKER` never parses, so it lands on
the syntax branch and leaves `n < 0` untested while looking covered — the negative and
overflow cases plant a distinctive numeric literal instead. Cost
is expressed in micros of the ad account's currency (consistent with the Google Ads metrics
path, so a platform-agnostic dispatcher can normalize all platforms to the same unit). CTR
is computed client-side (Clicks/Impressions, 0 when Impressions is 0 — never divides by
zero). The return type `CampaignMetrics` is distinct from the domain type
`model.CampaignMetrics` (an application-level platform-agnostic staging area), converted at
the dispatcher boundary.

## Credential scrubbing on error bodies

When an error response is NOT a Graph diagnostic — a proxy page, a WAF block, a
reflection of the request — the raw body is truncated into `APIError.Message`, and a
reflection can echo the `Authorization` header. Scrubbing runs in two passes, in this
order, and both are needed.

`Client.redactSecrets` replaces this client's OWN configured `Credentials.AccessToken`
by exact value first. That pass exists because the shape-based one cannot cover it:
`credentialRE`'s Bearer alternative matches `[A-Za-z0-9._~+/=-]`, while the access
token is accepted after trimming and nothing else. Meta's app access tokens are
`{app-id}|{app-secret}`, and `|` is outside that alphabet — shape-based redaction alone
turns `Bearer 12345|SECRET` into `Bearer [REDACTED]|SECRET`, carrying the app secret
through while wearing a redaction marker. Exact-value replacement cannot be defeated by
an unanticipated character. Secrets shorter than `minRedactableSecretLen` (8) are left
alone: at that length a substring replace matches ordinary prose and destroys the
diagnostic without protecting anything.

`redactCredentials` then runs the shape-based pass, which still matters — a reflected
body can carry credentials this client never held, such as a Meta-constructed
`paging.next` URL with its own `access_token`. It keeps the KEY and drops the VALUE,
and decides which alternative fired by scheme prefix rather than by delimiter search,
because base64 padding puts `=` inside a bearer token.

## Ad-account discovery (accounts.go)

`Client.ListAdAccounts` walks `GET /me/adaccounts?fields=id,name,account_status&limit=100`
and returns every ad account the access token reaches. It asks about the TOKEN, so
`AccountConfig` is not consulted — that is what lets it answer "which account should this
connection use?" for a connection that has not chosen one.

**An incomplete walk is an ERROR, never a short list.** Every one of the failure modes —
a 2xx body with no `data` field, a `next` link whose cursor is empty, a repeated cursor, an
id that is not `act_<digits>`, and the 20-page cap — returns `nil, error` rather than what
was collected so far. At the boundary a truncated list is indistinguishable from a complete
one, and the caller acts on the absence: the account they wanted is simply not offered, and
they conclude their token cannot reach it. That is the same false-absence discipline the
find-by-name lookups follow.

The `data` guard rests on a property of `encoding/json` that is easy to state backwards: a
PRESENT empty array decodes into a **non-nil** empty slice, while an absent or null field
leaves the slice **nil**. A plain slice therefore already separates `{"data":[]}` from `{}`,
as long as it starts nil — which is why the response struct is declared inside the page loop
rather than reused. `Data == nil` then means "this body carried no result set", so a
malformed `{}` cannot read as "fully enumerated, zero accounts". The
accumulator is `make([]AdAccount, 0, n)` for the mirror-image reason: a token that
legitimately reaches zero accounts is an ANSWER, and it has to stay distinguishable from
"no answer" all the way up to the HTTP body, where nil serializes as `null` instead of `[]`.

**`paging.next` is never followed.** Meta's own next-page URL carries `access_token` and
`appsecret_proof` as query parameters, so following it would put the credential into the
request URL and from there into `apiError`/`transportError` text that the discovery handler
logs. Each page's path is rebuilt locally from the opaque `after` cursor instead — the same
rule `listAdIDs` follows.

Known-bad accounts are RETURNED with their reason (`StatusLabel`, reading the same
`inactiveAccountStatusLabels` map `CreateCampaign`'s preflight refuses on), not filtered. This
feeds a picker, and a user whose only account is unsettled needs to see it and see why —
dropping it answers "your token reaches no ad accounts" about an account sitting right there.
The decision to REFUSE a campaign on such an account stays in that preflight, where it
already is. `Status == 0` means Meta omitted the field, not that the account is disabled.

## Publishability: Instagram identity and DSA disclosure

`CreateCampaign` builds an API-accepted hierarchy, but two additional fields decide
whether Meta will let a human PUBLISH the resulting ad. Both are optional
`CampaignInput` fields, trimmed once in `CreateCampaign` and attached only when
non-empty (so Facebook-only / non-regulated flows are unchanged and Meta never
receives an empty string it would reject):

- **`InstagramUserID`** (config `instagramUserId`) — the Instagram account (IGSID)
  bound to the ad creative as the top-level `instagram_user_id` adcreative field,
  SIBLING to `object_story_spec`, not nested inside it. The default placements
  include Instagram Feed, and an ad that requests an Instagram placement without this
  field is flagged "Please add Instagram account" and cannot be published — even
  though the Page's connected Instagram account shows pre-selected in the editor. The
  legacy Graph field name is `instagram_actor_id`. A SUPPLIED value must be a numeric
  IGSID (`numericIDRE`, the same gate `PageID` and `PixelID` get); a malformed one is
  rejected before the first mutating call, because the field is otherwise consumed only
  at the creative POST — which runs after the campaign and ad set exist and treats a 4xx
  as a non-fatal per-variant failure, leaving a `created_degraded` billable campaign with
  no publishable ad. An EMPTY value stays valid: that is a Facebook-only campaign.
- **`DSABeneficiary` / `DSAPayor`** (config `dsaBeneficiary` / `dsaPayor`) — the EU
  Digital Services Act advertiser/payer disclosures set on the ad set as
  `dsa_beneficiary` / `dsa_payor`. Meta blocks publish ("Please add Advertiser" /
  "Please add Payer") for regulated locations until both are present. The two attach
  independently, so exactly one could otherwise be sent; a ONE-SIDED pair is rejected
  before any mutating call, since Meta requires both to publish a regulated ad set and
  the incompleteness is knowable at validation time rather than only after a billable
  campaign exists. BOTH ABSENT remains valid — that is the ordinary non-regulated flow.

These live in the per-campaign config (like `pixelId`) rather than the connection's
persisted `providerConfig`, so no new stored column is required; a launch-ready
config must supply all three when the ad set uses Instagram placement and/or targets
a regulated location.

The distinction these guards draw is between a PERMANENT input fault and Meta's own
publish-time requirements. Whether a disclosure is REQUIRED at all depends on the
targeted location, which this client does not evaluate, so PRESENCE is still not
validated locally and a genuinely missing disclosure surfaces as an async publish block
on Meta. What IS validated is what is deterministically knowable here: a malformed IGSID
and a one-sided DSA pair are unpublishable regardless of targeting, so they fail
pre-create rather than after a paid resource exists. Both rejections are plain errors
returned with a `nil` result, which is what the dispatch adapter keys on to mark them
`NoUpstreamCreate` so the orchestrator RELEASES the claim (see below).

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` meta adapter (see [internal/dispatch](internal-dispatch.md))
interprets a single OAuth2 accessToken; AccountConfig comes from AccountID (`act_...`) +
`page_id` (REQUIRED by the connection design — the dispatcher needs it to attach the
promoted-object page, so requiring it at connection time surfaces a 4xx instead of a
silent dispatch failure). Budget is in the ACCOUNT's currency (no FX), with an optional
CurrencyOffset.

It implements `StatusToggler` and CASCADES: its create PAUSES the campaign, ad set, and
ads, so `UpdateCampaignAndChildrenStatus` POSTs the status to the campaign, the persisted
ad set id, and each ad DISCOVERED via `GET /{adSetID}/ads`. Since LFXV2-3295 the result blob
also records the created ad ids (`CampaignResult.Ads`, one entry per successfully-created ad),
so the discovery is a deliberate choice rather than a necessity: the live enumeration also
covers ads added to the ad set after dispatch, and still works for rows written before that
field existed. It needs only the access token, not the page id.

See [internal/platform/meta](../../../internal/platform/meta).
