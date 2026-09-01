# 2026-09-01 — LFXV2-2775: concept docs caught up with three behaviour changes

**Docs** — three behaviour changes shipped in this PR without their concept files following, and
the reviewer caught the gap twice: once for the concepts themselves, then again because the
concept fix landed with no dated log entry of its own. This is that entry.

What changed, and where it is now recorded:

- **`internal-audience.md`** — the HubSpot filter-shape invariants listed three rules; a fourth
  was learned against the live API and was missing. `operator` is required at the FILTER level as
  well as inside `operation`; omitting it fails the whole create with
  `Some required fields were not set: [operator]` — a 400 that names no field path, so it reads as
  a malformed request rather than a missing key.
- **`internal-service-email-copy.md`** — `emailCopyEventDetails` gained a `dates` field, the
  COMBINED free-text form the scraper produces. It is what UI-written briefs actually carry:
  `startDate`/`endDate` are paid-platform config fields and are empty on an email brief, so
  reading only the structured pair yielded "Date TBD" for every email while the real dates sat in
  the brief beside them. The inventory now names every test rather than counting them, after the
  count went stale at 20 against 24.
- **`internal-dispatch.md`** — generated-copy application (`applyEmailContent`) had no coverage at
  all: its best-effort ordering, why a failure is swallowed rather than returned, and the
  exactly-one-rich-text-widget guard that identifies a widget by the PRESENCE of the `body.html`
  key rather than a non-empty value.

**The general shape:** "update the concept" and "log the update" are two obligations, and
satisfying the first does not discharge the second. A reviewer asking for a doc fix will ask again
about its log entry, which is the cheapest possible second round to have caused.
