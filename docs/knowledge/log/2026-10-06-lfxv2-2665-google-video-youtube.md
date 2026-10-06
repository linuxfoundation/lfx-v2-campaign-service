# 2026-10-06 — LFXV2-2665 Video (YouTube) campaign creation

**Creation** — `video.go` and `video_creative.go` add the fourth Google Ads channel. The shell
is `advertisingChannelType: VIDEO` **with** `advertisingChannelSubType: VIDEO_ACTION` — `VIDEO`
alone covers reach and view products that bid toward entirely different things, so the sub-type
is part of the identity of what is created, not a decoration, and the wiring test asserts both.
The creative is a `VIDEO_RESPONSIVE` ad assembled over two mutates: each YouTube video id
becomes a `youtubeVideoAsset` in one `assets:mutate` read against the same POSITIONAL response
contract Performance Max uses, then one `adGroupAds:mutate` builds the ad. Ad group and ad are
created PAUSED like everything else this client creates, and both ids are reported even when
the ad create fails, under the partial-result contract.

**Note** — Video is the first channel whose creative costs this service **no network at all**.
A YouTube video is referenced by the id the caller already owns and Google already hosts, so
there is no fetch phase, no transport to harden, no byte cap and no URL to redact. That is why
`googleAdsSnapshotConfig`'s early return deliberately does NOT name `videoCreative` — a config
carrying only a video creative has nothing to sanitize — and why the whole Video preflight is
PURE, with every refusal decided before the first budget mutate rather than merely happening to
precede it.

**Note** — The bidding set is the narrowest pair `VIDEO_ACTION` documents —
`maximize-conversions` (the default) and `target-cpa` — and this client has **NOT** verified a
wider set against the live API, because no Video campaign has been created from here against a
real account. The refusal therefore says `NOT yet verified a wider set against the live API`
rather than borrowing the `only combination verified against the live API` wording Demand Gen
and Performance Max earned from recorded evidence.
`TestBiddingPlan_VideoRefusalDoesNotClaimLiveVerification` asserts both halves — that Video
carries the unverified clause and that the other two channels kept the verified one — because a
swap between them would otherwise be invisible. Conversion actions ARE accepted on Video, so
`campaign.selective_optimization` rides its shell exactly as it does on Search.

**Docs** — Two fences that name Demand Gen by name turned out to have no written reason, and
`video.go` pointed readers at one of them for an explanation that was not there. Video takes
campaign-level geo INCLUDING proximity and the whole un-narrowed campaign-criteria set — all
five kinds, exactly as Search does — so widening either fence to `!= campaignKindSearch` would
refuse campaigns Google creates happily: the over-refusal these guards exist to avoid.
`validateCriteriaPlan` and `geo.go`'s proximity fence now say so in place.

**Docs** — `VIDEO` was the worked example of a channel type adoption refuses, and is now one it
maps to its own slot. That is how the fail-closed list is meant to move: a type leaves the
refused set the moment a create path for it lands, and never before. `SHOPPING` and `HOTEL` are
the live examples now. `docs/api-catalog.md` gains a `videoCreative` field block, the `video`
campaign type, the per-channel bidding and criteria notes, and the corrected activation-gate row
— the gate lists Video alongside Demand Gen in one `case`, since keywords are refused on both
and a keyword gate there would be unsatisfiable by construction.
