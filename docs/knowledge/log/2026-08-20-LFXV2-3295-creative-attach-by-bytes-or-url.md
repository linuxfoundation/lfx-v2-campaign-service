# 2026-08-20 — LFXV2-3295 attach creatives by stored bytes OR by url

**Update** — the Meta client can now attach a variant's image by EITHER route, and
both survive:

- `AdVariant.ImageURL` → `link_data.picture` (main's shipped path, byte-for-byte
  unchanged: no upload, Meta fetches server-side);
- `AdVariant.ImageAssetID` → resolved to bytes at dispatch, POSTed to
  `/act_<id>/adimages` as a multipart file part named `source`, and the returned account-scoped
  hash attached as `link_data.image_hash`.

**Meta forbids both on ONE creative, not both in one codebase.** The
`AdCreativeLinkData` reference says of `picture`: "Specify this field or `image_hash`
but not both." That is a constraint on a single creative. Each variant independently
picks a route, and one campaign may mix by-URL and by-bytes variants. A variant
supplying BOTH is refused by `validateVariantImage` — locally, before any credential
is used or any upstream call is made — because there is no correct interpretation:
silently picking one would attach an image the caller did not choose. `objectStorySpec`
then re-encodes the exclusivity structurally (`picture` only when no hash was
uploaded), so the wire body cannot carry both even if a future path skipped the
validator.

**Correcting the record on `/adimages`.** The 2026-08-18 fragment and the concept doc
concluded "nothing in the repo calls `/adimages` any more". The *finding* behind that
was right and is untouched: the old code sent a `url` field, `url` is not a create
parameter on that edge (it is a field on the RETURNED image object), and the call
would have been rejected live after the campaign and ad set already existed. But the
verdict is about the `url` PARAMETER, and it was recorded as if it disqualified the
ENDPOINT. Re-checked against Meta's ad-account `adimages` reference before relying on
it: the edge documents exactly two Creating parameters, `bytes` and `copy_from`.
Sending the image as `bytes` is documented, and it is the ONLY documented way to
attach an image the service holds as stored bytes rather than as a URL. So the branch
that used `bytes` was never carrying the defect #144 fixed — a correct finding about a
parameter had been generalised into a false claim about an endpoint, and that claim
would have blocked the only viable transport for stored assets.

**The two designs did not conflict — one had DELETED the other.** The briefing said
these were competing implementations occupying the same lines. They are not. The
unmerged source branch REPLACED `picture`: it dropped `AdVariant.ImageURL`, dropped
`validateVariantImageURL` (with it the absolute/https/**userinfo** checks — and Meta
FETCHES that URL server-side, so embedded basic-auth credentials would be handed to
Meta inside the creative body), and replaced all four per-variant `Steps` renderings
of `scrubURLFromErr(verr, variant.ImageURL, 300)` with a plain `truncateErr(verr, 300)`,
retiring the pre-signed-URL scrubbing added by the 2026-08-18 and 2026-08-19 work.
That is why four of main's tests fail on a naive merge, and it is why coexistence is
ADDITIVE work rather than a reconciliation: nothing had to be traded away. A wholesale
import would have passed `go build`, `go vet`, `gofmt` and golangci-lint with `0
issues` while silently reinstating a credential leak into persisted `Steps`.

**The pre-spend guard did not bind, and two mutations that govern money survived.**
Asset resolution failing before any upstream create — so a bad image cannot spend
budget, and the (brief, platform) claim is RELEASED rather than stranded — was true
only BY INSPECTION. The three `TestMeta_ResolveVariantAssets_*` tests exercise the
helper in isolation, so nothing drove it through `Dispatch`; both of these survived
against them:

    notCreated(verr) -> verr                       (strands the claim)
    bad asset -> continue, build a link-only ad     (spends budget)

Neither is visible to a helper-level test: the first is about how the CALL SITE wraps
the error, the second about whether the call site fails at all. `TestMeta_DispatchBadAssetRefusesBeforeAnySpend`
now drives six bad-asset cases through the real `Dispatch` against a stub that FAILS
the test on any mutating request, asserting (a) zero upstream creates observed on the
wire, (b) `errors.As` finds `NoUpstreamCreate()` — the interface the orchestrator
actually consults, not the concrete `*preCreateError` — and (c) a nil campaign, so
nothing reads as "may exist". Both mutations now reach `POST /act_777/campaigns` and
fail on the wire rather than on an inference from an error value. The stranding
mutation is the more expensive one: per the claim contract, a stranded `pending` row
blocks every future dispatch for that pair and is recoverable only by a human, and it
cannot be safely deleted without an upstream check.

**One survivor, reported as benign.** Moving resolution from before `meta.NewClient`
to after it does NOT fail any test. `NewClient` is a pure constructor — it assembles a
struct, takes no context and makes no request — so no spend occurs either way, and the
property the tests bind is the observable one (no upstream create, claim released)
rather than statement order. Moving resolution past `CreateCampaign`, which is the
boundary that actually matters, IS caught. Recorded rather than papered over: the
ordering is kept as written because it is the clearer expression of intent, not
because a test forces it.

Mutation-tested with compiling reverts, each restored after — killed: stripping
`notCreated`; a bad asset falling through to a link-only ad; disabling the
both-supplied refusal; removing the `picture` branch (fails ALL FOUR of main's tests);
removing the `image_hash` branch (fails only the new test — main's four stay green,
which is what shows the paths are independent rather than merely coexisting in the
file); skipping the upload; discarding the resolved variants; dropping `ImageHash`
from `AdResult`; removing the empty-bytes guard; dropping the project/brief scope from
the asset lookup; and aliasing instead of copying the variants slice. Survived:
resolution moved after `NewClient` (benign, above).

Main's four shipped tests — `TestMeta_DispatchMapsVariantImageURL`,
`TestCreateCampaignAttachesPictureURL`, `TestCreateCampaignPerVariantImageIsolation`,
`TestCreateCampaignImageFailureIsNonFatalAndReported` — pass UNMODIFIED. They set only
`ImageURL`, never `ImageAssetID`, and their "`/adimages` must never be called"
assertions are scoped to URL-only variants, so coexistence keeps them true BY
CONSTRUCTION rather than by edit. Needing to edit one would have meant the coexistence
was wrong.

**Privacy: the by-bytes path has nothing to scrub, and that is by construction.** An
upload failure names only which variant failed — no bytes, no byte count, no checksum,
no snippet — so there is no caller-supplied value in its error text for a sink-side
scrubber to remove. The by-URL path keeps all four `scrubURLFromErr` sinks unchanged.
Campaigns are still created PAUSED and nothing here can publish or spend.
