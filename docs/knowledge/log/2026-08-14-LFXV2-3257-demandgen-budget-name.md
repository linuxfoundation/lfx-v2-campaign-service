# 2026-08-14 — LFXV2-3257 Demand Gen gets its own budget name

**Fix** — Search and Demand Gen on the same brief composed the SAME budget name,
so Demand Gen could not be created on a brief that already had a Search campaign.

The channel split gave the CAMPAIGN name a per-kind segment and left the budget
as a bare `"Budget"`. Every other `ComposeName` segment — Project, EventName and
the NameSuffix that carries the brief id — is identical for two channels on one
brief, so both composed `LFX | Budget | <project> | <event> | <brief id>`
byte-for-byte.

That name is this client's idempotency key: a non-shared budget's name is
deterministic in the brief precisely so a retry recomposes it and Google refuses
it with `DUPLICATE_NAME`, which the caller reports as already-exists rather than
creating a second budget. The collision therefore failed Demand Gen at the BUDGET
step — step 1 of the two-step hierarchy — and it never reached the campaign
create at all.

`budgetKindFor` is deliberately ASYMMETRIC: Search keeps the bare `"Budget"` and
only Demand Gen takes a channel-specific one. Renaming Search's budget would
break retry idempotency for every Search campaign already created upstream, whose
budget carries the old name — a retry would compose a name that does NOT collide
and would create a second budget for the same campaign. Demand Gen has no such
history, so it is free to take a distinct name.

**Note** — The first version of the test called
`ComposeName(budgetKindFor(kind), in)` directly and passed even after the call
site was reverted to the bare literal: it proved the helper works, not that
anything uses it. Both tests now go through `preflightCampaignKind` and read the
composed `budgetName` off the returned preflight, which is what makes reverting
the call site fail them. A test that exercises the helper instead of the path is
the shape that lets this class of defect ship twice.
