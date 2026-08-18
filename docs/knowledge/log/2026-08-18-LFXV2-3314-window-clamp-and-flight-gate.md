# 2026-08-18 — LFXV2-3314 the current day counted whole, and zero_delivery ignored the flight

**Fix** — two more paths where a figure was derived from the clock rather than from the campaign.

**A window cannot carry spend from time that has not happened.** `WindowInterval` returned the
whole calendar span, so `today` at noon contributed a full day of plan against half a day of
spend and reported a campaign spending exactly on plan as ~50%. The interval is now clamped to
`now`.

This clamp was REMOVED earlier in this ticket and is now back, deliberately, because the first
removal fixed the wrong half. The rolling windows do report their whole span with only the final
day partial, so clamping costs a little accuracy there — `last_7_days` at noon reads ~8% ahead.
But that bias is bounded by half a day out of the window's length and shrinks as the window
grows, whereas the unclamped error on `today` is a factor of two and worst early in the day when
an operator is most likely to look. Erring toward "spending ahead" is also the safer direction:
it never manufactures an underspending item against a healthy campaign.

**`zero_delivery` reasoned about delivery without reference to the flight.** A campaign can be
dispatched days before its start date; one scheduled for next week has delivered nothing for the
same reason it has spent nothing. The rule told an operator to check targeting and creative
approval for a campaign whose only property was being early. `Input.DeliveryExpected` now gates
it, wired from `rules.DeliveryExpected` which shares `ComputePacing`'s one-elapsed-day floor — so
the delivery rule and the pacing rules agree on when an absence is evidence rather than
reporting lag.

**A finding that was checked and rejected.** The same review claimed a campaign persisted as
`paused` could raise an underspending item. There is no `paused` campaign status: the model
defines `pending`, `created`, `created_degraded`, `deleted`, `group_created` and `unconfirmed`,
and the toggle deliberately does not persist run state. The `"paused"` strings in the tree are
all platform-side run state inside ad clients. Verifying before fixing mattered here — a change
to satisfy that finding would have been a real new defect.
