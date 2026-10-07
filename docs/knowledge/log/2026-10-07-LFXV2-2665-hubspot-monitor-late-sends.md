# 2026-10-07 — LFXV2-2665 HubSpot monitor late sends and deleted campaigns

**Fix** — Two PR #290 review threads on `monitor-hubspot-account`.

- **`emails_truncated` is unconditional.** It was cleared when the oldest checked campaign
  predated the window, but an older unchecked draft can be sent late, inside the window, and
  nothing stored records send times — so those totals were silently short. It is now true
  whenever the project has recorded more than 50 HubSpot campaigns. Model, port, query, design,
  catalog and concepts restated.
- **Soft-deleted campaigns are in scope.** A local delete neither stops nor deletes the HubSpot
  email, so `ListRecentProjectPlatformCampaigns` now keeps soft-deleted rows (the one read that
  does; its soft-delete test asserts the opposite of the live predicate) and the rows carry a
  required `deleted` flag. They get the same portal check; an email on both a deleted and a live
  row is owned by the live one. A project whose only campaign was deleted is now read rather
  than answered empty.
