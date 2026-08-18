# 2026-08-18 — LFXV2-3314 the catalog still described the old zero_delivery scope

**Docs** — `docs/api-catalog.md` enumerated the four action-item rule tokens as peers derived
"from the readable rows", with no qualification. The previous commit made `zero_delivery`
paid-ads only and mutually exclusive with the pacing items, and did not update the catalog.

The direction matters: the sentence was ACCURATE before that change and incomplete after it, so
the staleness was caused by the change rather than sitting near it. A consumer building on the
catalog would reasonably have written a UI expecting `zero_delivery` on an email row, or one
rendering a pacing item beside it — neither of which the service emits.

**The concept's rule TABLE had the same gap.** Its prose carried both constraints, but the table
above it still read "No impressions and no spend, on a campaign believed to exist upstream" — and
the table is what a reader scans. Fixing the prose and leaving the table is the fix-the-instance
mistake: the same claim lived on two surfaces of one file.

**The design enum was checked and left alone.** `Enum("zero_delivery", …)` is a token list, not a
claim about when each token fires, so it stays correct under both constraints.
