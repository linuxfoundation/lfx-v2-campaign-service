# 2026-08-18 — LFXV2-3314: low_ctr was the one rule not gated on run state

**Fix** — `docs/api-catalog.md` states the contract without exception: *"Every rule is gated
on the campaign's status ... a `paused` campaign raises nothing, because zero spend is the
intended outcome of pausing it."* Three of the four rules honoured it. `low_ctr` did not.

The consequence is small but wrong in a specific way: a paused campaign's historical CTR
raised a MEDIUM item telling an operator to refresh the creative on something they had
deliberately stopped. The rule set exists to suppress exactly that class of false alarm.

Reproduced before fixing — a paused campaign with 20,000 impressions and 0.1% CTR returned
one `low_ctr` item.

The gap survived because the existing run-state test used `CTRPct: 2`, above
`LowCTRThresholdPct`, so `low_ctr` could not fire in it either way and its missing gate was
invisible. The new `TestEvaluate_EveryRuleRespectsTheRunState` uses an input that would trip
EVERY rule if any gate were removed, and asserts the mirror case — the same input on an
`active` campaign must still raise, or the test would pass against a rule set that never
fires at all.
