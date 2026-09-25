# 2026-09-25 — the fallback partition ran on provisional dates and deleted the honest rows

**Fix** — the `eventMatches > 0` partition ran in the classification loop, before the
authoritative send date had been read. Two blocking defects followed from that, both
reintroducing the false empty history this endpoint exists to prevent.

**A — a SCHEDULED row deleted the brand fallback, then removed itself.**

The loop can only consult the PROJECTED `publishDate`, and `sentInTheFuture` treats an ABSENT
one as "not future" deliberately — an absent date must never exclude a row. So on a portal
that omits `publishDate` (one that ignores `includedProperties`), a `PUBLISHED_OR_SCHEDULED`
row booked for next month survives the loop as a real event match. The partition then deleted
every fallback row PERMANENTLY (`candidates = kept`), and the authoritative re-check later
dropped that same row as future.

Net result: zero rows. Reproduced with a two-row portal — one scheduled event match, one real
brand send — which returned `rows=[] err=<nil>` where the brand row was the honest answer.

**B — brand and generic-only fallbacks were ranked together by date.**

`Fallback` covers both tiers, and nothing downstream distinguished them, so a newer unrelated
`Registration Open Now` sorted ahead of an older brand row that was the correct precedent.
The two are not equal evidence: the brand is a last resort the operator chose via
`brand_short`, while a generic-only hit is an accident of an event name made of
portfolio-common words.

**The fix** — the partition moves AFTER the authoritative read, onto the survivors, and
becomes three tiers rather than two: a distinctive event match, else the brand, else
generic-only (`keepStrongestTier`). Fallback rows now ride through the shortlist so they
remain available to be promoted if the event matches evaporate. `ranked` carries `brandOnly`
alongside `fallback` to separate the tiers.

`eventMatches` is deleted: with the partition moved it was incremented and never read.

## Tests

`TestLastSent_AFutureEventMatchDoesNotDeleteTheBrandFallback` and
`TestLastSent_TheBrandFallbackOutranksAGenericOnlyHit`. Mutation-verified: merging the brand
and generic tiers fails the second; moving the partition back before the authoritative read
fails the second AND `TestLastSent_ABrandOnlyHitIsDroppedWhenTheEventItselfMatched`.

`TestLastSent_TheSendDateReadIsCappedAtItsCeiling` pins `maxSendDateReads` — the ceiling the
endpoint's documented cost rests on, previously unreachable through the API and untested. It
was added by the PRECEDING commit, not this one, and is noted here only because the two
review findings arrived together.
