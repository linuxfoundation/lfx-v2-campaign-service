# 2026-10-06 — LFXV2-2665 Display campaign creation

**Creation** — `display.go` and `display_creative.go` add the fifth Google Ads channel. The
shell is `advertisingChannelType: DISPLAY` with **no** `advertisingChannelSubType` at all,
and the wiring test asserts the ABSENCE rather than a value: Video is the sibling that pins
one, and a cascade copied from its shell would send `VIDEO_ACTION` on a DISPLAY campaign —
a different product. The campaign bids `maximizeConversions` by default, the ad group is
typed `DISPLAY_STANDARD` and created ENABLED (as Demand Gen's and Video's are, Search being
the only channel here that pauses its ad group), and the AD is created PAUSED under a
PAUSED campaign, so nothing serves either way.

**Note** — `longHeadline` is a **SCALAR** on this channel, not a list. A responsive display
ad takes exactly one, so `DisplayCreative.LongHeadline` is a `string` where Performance Max
and Video both carry `[]string`. That is the one place this channel's wire shape departs
from its siblings, and the failure mode is silent: a mapper copied from either compiles and
either drops the field or keeps only its first element. Both the dispatch mapper test and
the wiring fixture spell it as a string rather than reusing a sibling's helper.

**Note** — Display fetches image bytes, so unlike Video its preflight is not PURE — but the
fetch still runs BEFORE the first budget mutate, so a refused image strands nothing.
`fetchDisplayImages` goes through the same hardened transport Demand Gen and Performance Max
use (https only, no redirects, public-IP dial guard, 5 MiB per image and 64 MiB across the
creative, decoder-set format allowlist, geometry against the decoded image within the
documented ±1%). The four slots are marketing 1.91:1 ≥600x314, square marketing 1:1
≥300x300, logo 4:1 ≥512x128 and square logo 1:1 ≥128x128; at most 15 marketing images
COMBINED across the two marketing arrays, 5 logos per array. A marketing image **or** a
square marketing image is required and either satisfies the requirement — RECIPROCAL, as on
Demand Gen and unlike Performance Max where both shapes are required. That reciprocity is
why the each-field dispatch table omits the two marketing arrays TOGETHER: omitting one
alone is not a refusal, and a table that did it would pass against a mapper that never read
that array at all. Text counts are Display's own — 1–5 headlines (≤30 weighted), 1–5
descriptions (≤90), a required long headline (≤90), a required business name (≤25), an
optional call to action (≤30 runes, a payload bound this client sets rather than an upstream
limit, and the comment says so). Over-long copy is REFUSED, not truncated.

**Note** — Targeting is the un-narrowed Search set, exactly as on Video: campaign-level geo
INCLUDING proximity, and all five campaign-criteria kinds. Conversion actions are ACCEPTED,
so `campaign.selective_optimization` rides the Display shell — Google documents that field
for SEARCH, DISPLAY, VIDEO and APP campaigns. Keywords, audience segments, campaign-level
negative keywords, a CPC bid, extension assets and multiple ad groups are all REFUSED, by
the pre-existing `!= campaignKindSearch` fences that Display inherits correctly.

**Fix** — `conversionActions` was documented in `docs/api-catalog.md` as SEARCH ONLY while
`validateConversionActions` had already admitted Video and Display. That drift was left by
the Video round; the catalogue now says ACCEPTED on `search`, `video` and `display` and
gives the `selective_optimization` reasoning.

**Fix** — `CONNECTED_TV` was a named gap whose stated reason went stale the moment this
client gained the two channels Google supports it on. The old `deviceTypes` comment claimed
the device was useless on either channel this client creates and that the omission was "not
the over-refusal this package otherwise guards against" — true while Search and Demand Gen
were the only kinds reaching that map, false once Video and Display did. The set is NOT
widened, and the comment and the `validateDeviceBidModifiers` refusal now say why: the
documentation does not settle whether a CONNECTED_TV campaign criterion carries a BID
MODIFIER, this client has no way to send a criterion without one, and a wrong guess lands at
the `campaignCriteria:mutate` AFTER the budget and campaign are committed — precisely the
orphan the preflight exists to prevent — while the only reachable Google account is a
production one where a `validateOnly` mutate is still a POST. Under-refusing is safe;
stranding a paid campaign is not. The comment names the closing procedure instead of leaving
the next reader to rediscover it: run that `validateOnly` criteria mutate on a
non-production DISPLAY and VIDEO campaign, then admit the device for those two kinds alone.

**Note** — The other named gaps recorded with this change, each a limitation of THIS CLIENT
rather than of Google, and framed that way wherever the two differ: Google accepts MANUAL
CPC on Display, this client refuses it because the Display ad-group payload carries no bid
field and `cpcBid` is refused off Search; the five automated strategies it does accept are
**not live-verified**, on Video's terms and for the same reason; `youtubeVideos` on a
responsive display ad and the `pricePrefix`/`promoText` fields are not sent at all; and the
Display adoption slot is keyed on `advertising_channel_type` alone, so ANY `DISPLAY`
campaign fills it — one slot per channel TYPE is the model, not a Display-specific gap.

**Docs** — `docs/api-catalog.md` gains the `display` campaign type, a `displayCreative`
field block, the Display bidding paragraph, and the per-channel corrections the fifth
channel forces on the geo, languages, device, demographic, ad-schedule, conversion-action,
adoption-slot and activation-gate notes.
