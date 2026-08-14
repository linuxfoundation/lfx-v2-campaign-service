# 2026-08-14 — LFXV2-3257 resolve the channel before adoption and slotting

**Fix** — Three more review defects on #130, sharing one root cause: the Google
Ads channel was decided too LATE. The campaign name, the adoption lookup and the
slot key were all committed while still assuming Search, so each of them was
wrong for a Demand Gen dispatch.

The dispatcher composed `ComposeName("Search Campaign", …)` and ran the
`adoptExisting` lookup against that name BEFORE the channel switch selected the
create. A Demand Gen dispatch therefore searched for the SEARCH campaign's name.
Either it adopted a Search campaign into the `demand-gen` slot — leaving the
brief claiming a Demand Gen campaign that is nothing of the kind — or it missed
the real `DemandGen Campaign` and created a SECOND paid one. The channel is now
resolved and validated first, and an unsupported value is refused before the
client is contacted at all, where previously adoption could bind a campaign
before the switch ever rejected it. `CampaignKindSearch`/`CampaignKindDemandGen`
are exported so dispatch composes the same name the client will: a local literal
in the caller is one edit away from looking up a name the client never writes.

`variantForDispatch` returned `VariantDefault` when the config envelope failed to
decode, on the reasoning that the dispatcher should report the specific error.
It never got the chance — the idempotency lookup runs FIRST, so on a brief that
already held a Search campaign the malformed request matched that row and was
answered as a reused success. The caller was told a campaign it never validly
asked for had been created. `VariantInvalid` (`_invalid`) is a slot no create
writes, so the lookup always misses and the dispatch reaches the real error. The
leading underscore keeps it outside the namespace any provider channel string
could produce.

Adoption stored every Google campaign as `default` whatever type it was, because
`GetCampaign` never selected `advertising_channel_type`. Adopting a Demand Gen
campaign thus left the `demand-gen` slot free, and the next Demand Gen dispatch
for that brief created a second paid campaign. The lookup now reads the channel
type, `PlatformCampaignRef` carries a `Variant`, and the adopt path persists it
and re-checks occupancy on the ACTUAL slot. The mapping fails CLOSED: a type this
service cannot create — `PERFORMANCE_MAX`, `VIDEO`, a value Google adds next
quarter, or an absent field — is refused rather than defaulted, because
defaulting is precisely what creates the duplicate. Google Performance Max is
expected here, and it will be refused with a clear error until it has a slot of
its own rather than silently filed under someone else's.

**Note** — Each fix was mutation tested: reverting it individually makes its own
test fail with the intended message. The first attempt at the name-composition
mutation failed to COMPILE (an unused variable) rather than failing a test, which
proves nothing about coverage; it was redone as a mapping change that compiles.

`TestVariantForDispatch`'s comment asserted the old fallback was correct.
Changing the expected value while leaving that reasoning in place would have
preserved a claim now known to be wrong, so the comment was rewritten with it.

`targetSpend` for Demand Gen remains open and unchanged — see the previous entry.
