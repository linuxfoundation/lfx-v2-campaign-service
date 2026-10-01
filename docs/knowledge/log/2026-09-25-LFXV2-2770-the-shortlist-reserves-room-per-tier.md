# 2026-09-25 — the shortlist reserves room per tier, and the bounded guard runs twice

**Fix** — moving the fallback partition after the authoritative read (earlier today) fixed a
false empty history and created two more. Cursor Bugbot found both; each is the previous fix
taken one step too far.

**A — a busy brand evicted the event's own send before it could be read.**

With the partition moved, brand and generic-only rows competed for the same `limit + 12`
shortlist slots as distinctive event matches, in date order. A `brand_short` covers the whole
portfolio and publishes far more often than any one event, so 25 recent newsletters filled
every slot and the event's own older send was never read at all. `keepStrongestTier` then
chose the best of what survived — newsletters — and returned portfolio mail as "last sent"
precedent for the event.

Reproduced: 25 brand sends dated September plus one real event send dated January, at
`limit=2`, returned `[brand24 brand23]`.

**The tension, and why neither cut alone works.** Taking the strongest tier BEFORE the read
lets a provisional classification decide: the rows still carry only the projected date, and
`sentInTheFuture` passes an absent one, so a scheduled-but-unsent event match still looks like
an event match and would again delete the fallback rows. Taking rows in date order lets the
brand evict the event. `shortlistAcrossTiers` gives each tier room instead: the strongest
takes what it needs, whatever remains is offered to the next. An event match can never be
evicted by a newsletter, and a fallback row is always still in hand if the event tier
evaporates.

**B — the bounded guard could not see a shortlist emptied by the authoritative read.**

The guard runs before the fan-out, so it missed the case where every shortlisted row is then
dropped as a booked send at the authoritative future gate. A bounded walk ending that way is
the same false absence for the same reason — the portal was never read to the end — so the
guard now runs again on `shortlisted`.

## Tests

`TestLastSent_ABusyBrandDoesNotEvictTheEventsOwnSend` and
`TestLastSent_ABoundedSweepEmptiedByTheAuthoritativeReadIsNotAnEmptyHistory`.
Mutation-verified: replacing `shortlistAcrossTiers` with a plain date-order truncation fails
the first and only the first; disabling the second guard fails the second and only the second.

Both earlier scenarios still hold simultaneously — a scheduled event match does not delete the
brand fallback, and a busy brand does not evict the event's send.
