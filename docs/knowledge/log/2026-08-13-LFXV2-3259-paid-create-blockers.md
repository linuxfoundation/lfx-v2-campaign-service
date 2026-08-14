# 2026-08-13 — LFXV2-3259 paid create blockers

**Fix** — Paid campaign creation through the UI cutover could not succeed for
any project, on any ad platform. Two independent causes, both found by running a
real create against a real Google Ads connection rather than by review.

**1. The dispatcher read a key the UI never writes.** `decodeBriefFields`
(`internal/dispatch/reddit.go`) decoded `event_details` into `briefFields`,
whose only name field was `json:"eventName"`. The UI writes `name` — its
`CampaignEventDetails` interface has spelled it that way throughout, and the
persist path spreads the object verbatim. `event_details` is `Any` in the design
(`design/brief.go`), so nothing arbitrates the key and neither side was wrong.
Every paid create decoded an empty `EventName` and was refused pre-create with
`brief %s has no eventName in its details`. Now accepts `name` as a fallback,
`eventName` still winning, with emptiness tested semantically (`TrimSpace`) so a
whitespace-only `eventName` does not block the fallback. Reader-side, so it also
repairs briefs already stored. Shared by every ad dispatcher, so LinkedIn, Meta,
Reddit and Twitter were blocked identically and are unblocked by the same
change.

**2. The keyword cap sat below the brief generator's own output.**
`internal/platform/googleads/targeting.go` capped keywords at 20, documented as
a sanity bound and explicitly not a Google limit. The AI brief generator emits
~38, so the default brief was rejected with `at most 20 keywords are supported,
got 38`. A cap the system's own upstream stage exceeds by default blocks creates
the platform would have accepted. Raised to 60 — at most 80 operations in one
`adGroupCriteria:mutate`, so the "keep one call a sane size" rationale still
holds. `maxAudienceSegments` unchanged at 20.

`internal/service/audience_build.go`'s `decodeEventDetails` carried the same
`eventName`-only defect in a separate struct, blocking the email channel the
same way (`BuildAudience` is the gate the HubSpot dispatcher waits on). Fixed
alongside; its doc comment claimed to mirror `decodeBriefFields`, which fixing
only the dispatch side would have made false.

**Note** — `countryCode` is deliberately NOT accepted as a fallback for
`country` in that decoder, even though the UI writes it and its absence is the
other reason a UI brief fails there. `Country` flows through
`audience.DisplayName` into HubSpot `CONTAINS`/`IS_ANY_OF` filters, and that
function aliases only `us` and `uk` among ISO-2 values
(`internal/audience/region.go`). A `JP` or `DE` would pass through literally,
match no HubSpot country property, and the build would succeed while storing an
empty inclusion list — a silent wrong answer on the list that decides who
receives an email. Accepting it needs an ISO-2 → country-name mapping first;
until then, failing with `no country in its details` names a real gap instead of
hiding it.

Verified end-to-end after both fixes: `platform_campaign_id 24140121766` created
on Google Ads, after six prior attempts produced zero campaign rows.
