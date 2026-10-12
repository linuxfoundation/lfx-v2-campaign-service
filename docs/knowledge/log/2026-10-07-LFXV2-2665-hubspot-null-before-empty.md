# 2026-10-07 — LFXV2-2665 HubSpot null counter refused before the empty-list answer

**Fix** — One more PR #290 review thread on the shared HubSpot statistics read.

`readEmailCounters` returned `ErrNoSentEmailInWindow` for an empty `emails` list before the
explicit-null guard ran, so `{"emails":[],"aggregate":{"counters":{"bounce":null}}}` was counted as
"not sent" although both the monitor and the per-campaign contract say a null counter fails
closed. The null guard now runs first. `TestStatistics_ExplicitNullCounterIsRefused` gains an
empty-list case on both reads.
