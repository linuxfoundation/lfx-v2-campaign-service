# 2026-09-03 — LFXV2-3198: a column default that could never apply

**Fix** — 000030 adds `delivery_type TEXT NOT NULL DEFAULT 'paid-marketing'` with a CHECK, and
`createBriefQuery` names the column explicitly in its INSERT. A column DEFAULT applies only when the
column is OMITTED from the insert, so for every write through this repo the default was dead: a
`model.CampaignBrief` built without setting `DeliveryType` sent the Go zero value `''`, which the
CHECK rejected.

Nine live tests broke at once, and none of them locally: every test in `dbtest` skips when
`TEST_DATABASE_URL` is unset, so a laptop run reported success while CI failed. The service layer's
`deliveryTypeOrPaid()` hid it further — it covers the HTTP path, so only a direct Go caller (which
is what those tests are) could reach the raw zero value.

Normalisation moved into `CreateBrief`, on the path every writer shares. Only the ZERO value
defaults: an unrecognised non-empty value is passed through so the CHECK rejects it.
`TestLiveCreateBriefDefaultsOnlyTheZeroDeliveryType` pins both halves, and the second half is the
one that matters — coercing a bad value to paid would not correct the typo, it would aim the write
at `(project, slug, paid-marketing, '')`, which under the widened key is the REAL paid brief's slot.

**The general shape:** a column DEFAULT is not a guarantee that the column is populated; it is a
guarantee only for inserts that omit it. If application code names every column — and generated or
hand-written INSERTs usually do — the default is documentation, not enforcement. And a suite that
skips on a missing environment variable reports "ok" for tests that never ran: check the PASS count,
not the exit code.
