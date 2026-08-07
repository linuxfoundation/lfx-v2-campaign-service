# 2026-08-08 — LinkedIn metrics: the findings that were never posted as threads (LFXV2-2994)

Copilot's reviews on #73 reported "no new comments" while burying six findings inside
`<details><summary>Suppressed comments</summary>` blocks in the review BODY. An unresolved
thread count of zero said nothing about them. Four were real and are fixed here; two were
already stale.

**Window validation now precedes credential resolution.** `linkedin.ValidateMetricsWindow`
is clock-free and package-level for exactly this reason: the dispatcher can reject
`yesterday`/`last_14_days` without a `Client`, credentials, or a network call. Order is the
whole point. An unsupported window is a permanent 400 whatever state the connection is in,
but resolving credentials first meant a project with an inactive or incomplete connection
failed with a connection error that `BriefService` maps to 503 — telling the caller to
retry a request that can never succeed. The X adapter already validated in this order.
`dateRangeForWindow` now calls the same validator first, and
`TestValidateMetricsWindowMatchesDateRangeForWindow` iterates the whole
`model.MetricsWindow` vocabulary so a newly added window cannot land in one and not the other.

**Clicks with zero impressions is rejected; per-metric presence tracking is not the fix.**
Copilot proposed `*int64` fields so a missing `impressions` key could be distinguished from
a real zero. That over-reaches: an omitted-because-zero key and an omitted-because-malformed
key are indistinguishable, so requiring every key would reject responses that are genuinely
fine. What stays decidable either way is the relationship — a click is preceded by the
impression that carried it — so an element reporting clicks with zero impressions is now a
read failure instead of a 200 carrying `Ctr=0` beside a non-zero click count.

**`costInUsd` is length-bounded before it reaches `big.Rat`.** The 10 MiB response cap does
not bound a single decimal, and `SetString`, the 1e6 multiply, and `FloatString(0)` are
super-linear in digit count and never observe the request context — so they keep burning CPU
after the 20s call deadline. `maxCostDecimalLen` (40 bytes) sits far above the largest real
figure int64 micros can hold (~9.2e12 USD, 13 integer digits) and far below anything useful
to an attacker. The test asserts the LENGTH error specifically, since a later overflow
rejection would mean the bound never fired.

**Two test-quality findings were opposite in kind.** The vacuous negative assertions
(`contains(prefix) && !contains(prefix)` — a pair that cannot fail) were already replaced
with exact equality in `6e080a5e`. Still open were: the analytics path builds its own
request instead of going through `doRequest`, so its hand-set `Authorization`,
`LinkedIn-Version`, and `X-RestLi-Protocol-Version` headers had no coverage at all; and the
happy-path test unescaped the whole query before asserting, which erases the exact
distinction `makeAdAnalyticsRequest` depends on — Rest.li structure LITERAL, URN values
ESCAPED — so a `url.Values.Encode()` rewrite would have decoded to the same text and passed.
Both are now asserted against `RawQuery` and the captured headers.

The general lesson is the review-reading one, not the LinkedIn one: on this repo,
`unresolved == 0` is not evidence that there is no open feedback. The review bodies have to
be swept.
