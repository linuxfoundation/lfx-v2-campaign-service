# 2026-09-09 — LFXV2-2775: the draft write has a lost-update window, and it is documented not closed

**Note** — `SetEmailHTMLWidgets` reads the draft and then PATCHes the whole `content` object back.
Those are two requests, so an operator editing the draft in HubSpot between them has their edit
overwritten by the snapshot this call took — and because the entire `content` object is re-sent,
the overwrite covers the whole draft, not only the block being written.

This window is NEW in this change. On `origin/main` the function issued a single PATCH of just the
changed widgets, with no read at all.

Neither half is removable on its own:

- **The read cannot be dropped.** A partial `content` PATCH destroys a drag-and-drop draft outright
  — HubSpot treats submitted content as authoritative, and a two-widget PATCH of a 33-widget email
  left the operator with 14 empty sections. The alternative to a narrow race is a guaranteed loss.
- **A conditional write has nothing to condition on.** Marketing Emails v3 documents no ETag, no
  `If-Match`, and no revision field on the draft endpoints. `Email.UpdatedAt` exists on the email
  resource, but whether the DRAFT GET returns it was NOT verified — no live credential was
  available in the session that raised this. A concurrency guard built on an assumed field would
  look like protection without being any, which is worse than a documented window.

So it is stated in the function contract and in the api-catalog `bodyHtml` entry rather than
silently accepted: an operator edit made between staging and dispatch may be reverted.

Closing it properly needs a live-API answer about draft versioning — whether the draft GET returns
`updatedAt`, and whether a re-read immediately before the PATCH is worth the extra request as
DETECTION (it narrows the window; it cannot exclude the race). That belongs in its own change.

Raised by review on #207.
