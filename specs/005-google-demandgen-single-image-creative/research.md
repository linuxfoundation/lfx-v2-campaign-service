# Phase 0 — Research: Google Demand Gen single-image creative

**Feature:** `005-google-demandgen-single-image-creative`
**Purpose:** Pin the exact Google Ads **v23** resource shapes, required-field sets, and
image constraints the G2/G3 code must produce, BEFORE writing that code — the same
de-risking the `targetSpend` probe did for the Demand Gen campaign create (LFXV2-3257,
verified 2026-08-14). Every "Decision" below feeds a specific plan step (G1–G3); every
open item in [§7](#7-open-live-verification-gates-must-run-before-g3-lands) is a live
`validateOnly` check that MUST pass before the corresponding code lands.

The client already runs at `v23` (`internal/platform/googleads/client.go:55`,
`googleAdsAPIVersion = "v23"`). All resource/field names below are v23 REST/JSON
(camelCase), matching the existing `demandGenCampaignCreate` / `adGroupAdCreate` payloads.

---

## 1. Headline finding — "single image" is a 3-role multi-asset ad, not one image file

Unlike Meta (where a single-image creative is literally one `image_hash` in an
`objectStorySpec.link_data`), **Google Demand Gen has no dedicated single-image ad
type.** The non-carousel, non-video Demand Gen ad is the
**`DemandGenMultiAssetResponsiveDisplayAd`** (v23 JSON field
`demandGenMultiAssetResponsiveDisplayAd` on the `ad` object). Google composes it into
image placements across YouTube / Discover / Gmail / Display, and it requires image
assets in **three distinct aspect-ratio roles**:

| Role | Aspect ratio | Min dimensions | JSON field |
|------|--------------|----------------|------------|
| Marketing image (landscape) | 1.91:1 | 600×314 (rec. 1200×628) | `marketingImages` |
| Square marketing image | 1:1 | 300×300 (rec. 1200×1200) | `squareMarketingImages` |
| Logo | 1:1 | 128×128 (rec. 1200×1200) | `logoImages` |

A single uploaded file cannot satisfy all three aspect ratios, so **"single image
creative" for Demand Gen means "a single non-carousel/non-video image ad", NOT "one image
file".** This is the material difference from the Meta slice and it changes G1 and G3
(see Decisions D1 and D4).

> **Consequence for the plan:** G1's `CampaignInput` creative fields must carry up to
> **three** asset references (marketing / square-marketing / logo), each an id into the
> reused `creative_assets` store — not a single `imageAssetId`. The plan's G1 line already
> anticipates multiple roles (`imageAssetId`/`logoAssetId`); this research pins the exact
> set to three and names them. The `creative_assets` table itself is unchanged — it stores
> role-agnostic bytes; the role is assigned by which `CampaignInput` field the id lands in.

---

## 2. Decision D1 — ad resource type

**Decision:** Build the v23 `DemandGenMultiAssetResponsiveDisplayAd` as the single-image
format. The G3 `demandGenAdBuilder` produces this shape for the `single_image` media
format key.

**Rationale:** It is the only Demand Gen ad type that renders as a static image creative.
The `ad` create wrapper is the same `adGroupAds:mutate` → `adGroupAdCreate{ad: adCreate{…}}`
plumbing the Search RSA path already uses (`adgroup_ad.go`); only the ad-type payload
inside `adCreate` differs (a new `demandGenMultiAssetResponsiveDisplayAd` sibling to the
existing `responsiveSearchAd` pointer, `omitempty`).

**Alternatives rejected:**
- `DemandGenCarouselAd` — the **carousel** media type (a future G-slice, FR-006/SC-005).
- `DemandGenVideoResponsiveAd` — the **video** media type (future).
- `DemandGenProductAd` — retail/Merchant-Center feed ads; out of scope (no product feed).

---

## 3. Decision D2 — image asset upload (`assets:mutate`)

**Decision:** G2's `uploadImageAsset(ctx, bytes, checksum) (assetResourceName, err)` posts to
`customers/{cid}/assets:mutate` (via the existing `customerPath` + `doRequest`,
`idempotent=false` — a create) with one create operation:

```json
{ "operations": [ { "create": { "imageAsset": { "data": "<base64(bytes)>" } } } ] }
```

The 2xx response is a single-id resource — `customers/{cid}/assets/{assetId}` — so the
existing `firstResourceName` + `validateResourceKind("assets", …, true)` extraction works
unchanged (contrast the composite `{adGroupId}~{adId}` shape `adGroupAdID` handles).

**Rationale:** Google Ads image assets carry bytes **inline as base64** in JSON — there is
no multipart endpoint (that was Meta's `/adimages`). `ImageAsset.data` is the documented
field. Reuses all of the client's request plumbing, timeout, and 429/ambiguity
classification.

**Alternatives rejected:**
- Multipart upload (Meta's shape) — Google Ads has no such endpoint for assets.
- Setting `Asset.type: "IMAGE"` explicitly — `type` is OUTPUT_ONLY on the `Asset`
  resource; providing `imageAsset` implies it. **Verify** whether sending `type` is
  ignored vs. rejected (§7 open item O2); default to omitting it.

---

## 4. Decision D3 — idempotency & partial-result contract

**Decision:** `uploadImageAsset` is a **non-idempotent** create: NOT retried on 429 (same
rule as every other `:mutate`, `doRequest(..., idempotent=false)`), and its outcome is
classified through the existing `createOutcomeAmbiguous` path. Idempotency across
*retries of the whole dispatch* is provided by the app side: we hold the SHA-256
`Checksum` in `creative_assets` and can skip re-upload, mirroring how the Meta client keys
on the same checksum.

**Open (O3):** Google Ads content-addresses image assets server-side, so re-`create` of
identical bytes MAY return the same resource name (no error) OR MAY reject as a duplicate.
This decides whether G2 needs to catch a "duplicate asset" code or can rely on Google
returning the existing resource name. **Must be confirmed by live probe before G2 lands.**

**Partial-result:** the asset upload extends the existing
`namePartial`/`budgetPartial`/`campaignPartial` chain in `CreateDemandGenCampaign` — an
asset created but then an ad-create failure returns a non-nil result carrying the asset
resource name(s), so nothing is stranded un-reconcilable (FR-007). This matches the
contract already proven for budget→campaign→adGroup in `demandgen.go`.

---

## 5. Decision D4 — ad payload required-field set

**Decision:** The G3 builder assembles a `demandGenMultiAssetResponsiveDisplayAd` with:

| Field | Source | Constraint (v23) |
|-------|--------|------------------|
| `businessName` | new `CampaignInput.BusinessName` (G1) | **required**, ≤ 25 chars |
| `headlines` | reuse `composeAdCopy` output | ≤ 40 chars each; **cap count at 5** (see below) |
| `descriptions` | reuse `composeAdCopy` output | ≤ 90 chars each; ≤ 5 |
| `marketingImages` | landscape asset → `{asset: <resourceName>}` | ≥ 1 (see D1 table) |
| `squareMarketingImages` | square asset → `{asset: <resourceName>}` | ≥ 1 |
| `logoImages` | logo asset → `{asset: <resourceName>}` | ≥ 1 |
| `finalUrls` (on `adCreate`, not the ad-type) | reuse `buildAdFinalURL` | ≤ 2084 bytes (`maxFinalURLBytes`) |
| `callToActionText` | optional; omit for now | — |

**`composeAdCopy` reuse is safe with ONE cap adjustment.** RSA weight caps are
headline 30 / description 90 (`ad_copy.go:31,34`); Demand Gen allows headline **40** /
description 90 — so RSA-compliant copy is strictly within Demand Gen limits (headroom on
headlines, exact on descriptions). RSA also produces min 3 headlines / 2 descriptions,
which satisfies Demand Gen's min of 1/1. **BUT** `composeAdCopy` allows up to
`maxHeadlines = 15`, while Demand Gen accepts at most **5** headlines. The G3 builder must
therefore **truncate the composed headlines to 5** before emitting the payload (descriptions
cap at `maxDescriptions = 4` ≤ 5, so descriptions need no extra cap). Do not widen
`composeAdCopy`'s constants — they are shared with the Search path; cap locally in the
Demand Gen builder.

**Precompute-before-mutate (FR-008):** all ad input (business name present + ≤25,
each image role resolvable, ≥1 of each required role, final URL valid + ≤ limit, copy
usable) is validated in a `precompute`-style step BEFORE the budget mutate, mirroring
`precomputeAdGroupAdInputs` — so a local input failure never orphans a created
budget+campaign+adGroup.

**Open (O1):** the EXACT required-vs-optional split among the three image roles (does v23
reject an ad with a landscape marketing image but no square, or vice-versa? is
`portraitMarketingImages` 4:5 ever required?) and the current char limits must be confirmed
by `validateOnly` before G3 asserts them (§7).

---

## 6. Decision D5 — channel stamping & activation gate (already pinned in plan)

`CampaignResult.Channel` carries the existing dispatcher constants
`googleAdsChannelSearch = "search"` / `googleAdsChannelDemandGen = "demand-gen"`
(`internal/dispatch/googleads.go:59-60`), stamped by both create paths, and the G5
ACTIVATE gate keys on it (demand-gen → ad+assets provisioned; search → ≥1 keyword; empty →
keyword fallback). No new research needed — see `plan.md` G3/G5 and the two review-fix
commits (`96fd4c86`, `07f41c13`).

---

## 7. Open live-verification gates (MUST run before G3 lands)

These are the exact analogue of the 2026-08-14 `targetSpend` `validateOnly` probe. **They
require a real Google Ads account + developer token, which this workstation does not have
configured** (only Snowflake and HubSpot are wired here) — so they are **NOT yet done** and
are tracked as blocking pre-code actions, not completed verifications. Running them needs a
throwaway `validateOnly` harness: add `ValidateOnly bool \`json:"validateOnly,omitempty"\``
to `mutateRequest` (Google Ads honors `validateOnly` on every `:mutate`) and point it at a
sandbox/real account, exactly as the `targetSpend` check was run.

- **O1 — ad shape & required set.** `validateOnly` `adGroupAds:mutate` with a
  `demandGenMultiAssetResponsiveDisplayAd` under a real Demand Gen ad group: confirm the
  JSON field name, which image roles are mandatory, and current headline/description/
  business-name char limits. Feeds D4/D1.
- **O2 — asset create shape.** `validateOnly` `assets:mutate` with
  `{imageAsset:{data:<base64>}}`: confirm it is accepted WITHOUT an explicit `type`, and
  that the response resource is `…/assets/{id}` (single-id). Feeds D2.
- **O3 — asset idempotency.** Create the SAME image bytes twice (non-validateOnly, sandbox):
  observe whether Google returns the existing resource name or a duplicate error. Decides
  D3's error handling.
- **O4 — targetSpend still 200 at current v23.** Re-confirm the campaign create the ad now
  hangs off still validates (guards against a v23 point-release regressing the
  already-verified `demandgen.go` payload).

**Until O1–O3 are green, G2/G3 code is written to this research's best-known shapes but is
NOT considered launch-verified.** Whoever has account access runs the probes and records
results in a dated `docs/knowledge/log/` fragment (per CLAUDE.md), the same way the
`targetSpend` result is recorded inline in `demandgen.go:134`.

---

## 8. What does NOT change

- **No new table / migration / upload endpoint / repo / handler.** `creative_assets`,
  `CreativeAssetRepo.CreateAsset`/`GetAsset(projectID, briefID, assetID)`, the
  `POST /projects/{project_id}/briefs/{brief_id}/creative-assets` upload, and the
  `model.CreativeAsset{Bytes, Checksum, MimeType, ByteSize}` shape are all reused verbatim
  from the Meta branch. The model's doc comment ("that a Meta ad creative references") is
  the only creative_assets touch owed, and that is a G7 docs-generalization, not a code
  change.
- **The PNG/JPEG allow-list matches Google.** Demand Gen images accept PNG/JPEG (+ static
  GIF); `creative_assets` already restricts to PNG/JPEG, a subset — no new MIME handling.
- **Geo stays out**, exactly as `demandgen.go` documents — geo parity is a separate,
  both-channels change (FR out of scope here).

---

## 9. Traceability

| Decision | Plan step | Spec FR |
|----------|-----------|---------|
| D1 ad type / 3 roles | G1 (`CampaignInput` fields), G3 (builder) | FR-001, FR-002 |
| D2 asset upload | G2 (`uploadImageAsset`) | FR-003 |
| D3 idempotency / partials | G2, G3 | FR-007 |
| D4 required fields / precompute | G3 | FR-004, FR-005, FR-008 |
| D5 channel gate | G3 (stamp), G5 (gate) | FR-009 |
| Reuse (no new storage) | G4 (dispatch resolve) | FR-010, FR-011 |
| O1–O4 live gates | pre-G2/G3 | SC-001…SC-006 acceptance |
