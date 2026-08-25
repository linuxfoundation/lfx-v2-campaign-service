# 2026-08-24 — a sweep keyed to one wording misses the paraphrase

**Fix** — a follow-up on PR #154, and a correction to the sweep recorded in
[2026-08-24-LFXV2-3067-a-missing-column-was-never-the-reason](2026-08-24-LFXV2-3067-a-missing-column-was-never-the-reason.md).
That fragment's conclusion is intact; its claim to have swept the whole class was not.

## What survived, and why

The earlier pass corrected the false "the field names match the campaign row's column names"
claim in `design/brief.go`, `docs/api-catalog.md` and the knowledge concepts. It reported the
class as swept. It was not: the leading comment on the settings-field const block in
`internal/dispatch/googleads.go` still said

> They match the campaign row's own column names, because the whole report is "what this row
> records" against "what the platform holds"

The grep that drove the sweep was keyed to the phrasing the flagged sites used — "column names
rather than", "no column on the campaign row expresses". This site says "match the campaign
row's own column names". Same claim, different words, zero matches.

The result was worse than leaving it alone. The Goa contract had just been corrected to warn
consumers that the vocabulary is explicitly NOT a column list, so a reader comparing the
contract against the adapter got **opposite answers about the same set of names**. A partial
sweep of a consistency class does not leave the remaining sites as they were; it converts them
into contradictions.

## The second site, found only by reading

Re-sweeping by MEANING rather than by string — reading every comment in the readback region
and checking each assertion about where a name comes from — turned up one more, which no
wording-based grep for "column" would ever have caught:

> Advertising channel type. This one IS recorded, unlike the **four genuine** upstream-only
> observations below

Four fields do pass a nil recorded side. But after the same-day change that gave `status` its
own rationale, only THREE are "genuine" in the sense that phrasing now means — nothing to
record. `status` is nil for the different-axis reason. The word `genuine` was accurate before
the fix and falsified by it. A fix can turn a true sentence elsewhere into a false one.

Note the count `four` is still CORRECT in `design/brief.go`'s Example comment, which counts the
REPORTED upstream-only set and derives the floor of six from it (4 + 2 flight dates). The same
number is right in one place and wrong in another because the two are counting different sets.
Changing it everywhere would have broken the arithmetic.

## Rule

**Sweep a consistency class by meaning, not by string.** Enumerate what the claim ASSERTS —
here, "where does each field name come from" — then read every comment in the affected region
and check it against reality, rather than grepping the wording the reported site happened to
use. A paraphrase is the common case, not the edge case.

And when a fix changes what a word means, re-read the neighbours that use it. The ground truth,
verified against the DDL: six of the ten names coincide with a `campaigns` column;
`advertising_channel_type` is COMPARED but recovered from `config_snapshot`;
`budget_delivery_method`, `budget_explicitly_shared` and `bidding_strategy_type` have neither
column nor recorded side.
