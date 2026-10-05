# 2026-10-05 — Dropping TV screens from the accepted device vocabulary

**Fix** — `deviceTypes` (`internal/platform/googleads/campaign_criteria.go`) accepted
`CONNECTED_TV`, a device Google supports only on Display and Video campaigns. Google's
own device-targeting documentation is unambiguous about TV screens: "This targeting
option is only available for Display and Video campaigns." Every criterion in that file
is refused outright on Demand Gen by `validateCriteriaPlan`, so Search is the only kind
that ever reaches the map — and Search is not a type the device works on.

So the knob was inert at best and, at worst, a rejected `campaignCriteria:mutate` that
lands AFTER the budget and the campaign are committed. The vocabulary is now MOBILE,
DESKTOP and TABLET.

**Why this is not the over-refusal this package guards against.** Over-refusal means
failing a create Google would have accepted AND meant. A device criterion Google
documents as unsupported on this campaign type has no such create behind it: if upstream
accepts it, it accepts it inertly, and the only thing a caller loses by being refused
here is a setting that never did anything. The asymmetry runs entirely one way — the
other branch costs a real paid campaign.

**The cap now tracks the vocabulary.** `maxDeviceBidModifiers` moved from 4 to 3,
because its comment makes a claim stronger than a bare bound: a longer list "is
necessarily a duplicate". That holds only while the cap equals the number of devices
accepted, so `TestMaxDeviceBidModifiers_MatchesTheAcceptedVocabulary` asserts the two
agree rather than leaving the next editor of either one to notice. An over-refusal guard
test pins the other direction: all three supported devices still fit in one list, and
that list is exactly as long as the cap allows.

**A note on how this was missed.** The 2026-10-03 local-review entry records reconciling
the device list across the client, the dispatcher comment and `docs/api-catalog.md`,
which had disagreed about how many values there were. That fix made all three say
"four, including CONNECTED_TV" — it corrected the COUNT without ever asking whether the
fourth value belonged. A consistency check between three documents proves only that they
agree with each other; none of them was checked against Google.

**Docs.** `docs/api-catalog.md` is the consumer-facing validation contract for
`CreateCampaigns.config` (typed `Any` in `design/`), so the narrowed vocabulary, the new
cap and the reason there is no TV-screen value all landed there; the googleads concept
doc and the dispatcher's wire-type comment carry the same account.
