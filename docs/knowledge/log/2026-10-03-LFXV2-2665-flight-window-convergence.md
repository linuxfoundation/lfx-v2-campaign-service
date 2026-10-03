# 2026-10-03 — Merging main's flight window into the Search-readiness branch

**Note** — `#241` (LFXV2-2023) and this branch's own flight-window commit
(LFXV2-2665) are the SAME feature, built independently and in parallel, and they
converged almost line for line: both take `startDate`/`endDate` as `YYYY-MM-DD`
under identical JSON names, render them into v23's `startDateTime`/`endDateTime`
with `00:00:00` / `23:59:59` day boundaries, resolve them in the shared preflight
BEFORE the budget mutate, enforce only the end-before-start rule, deliberately
accept a past start date citing the ad account's timezone, and send the window on
BOTH channels. This entry records which one survived the merge and why, so the
next reader does not have to re-derive it from two diffs.

**The branch's implementation survived.** Two differences decided it, neither of
them authorship:

1. **The snapshot is sanitized.** `applyCampaignConfig` writes `config_snapshot`,
   which is persisted UNENCRYPTED. The branch passes
   `googleAdsSnapshotConfig(cfg)` at both the create and the adoption call site;
   `#241` passes the raw `cfg`. Taking main's side would have reintroduced PR #239
   review finding #5 on the very call sites that fix it. (The same raw-`cfg`
   pattern still stands at `linkedin.go` and `microsoft.go` and is its own ticket,
   not this merge's.)
2. **The shape check is a regex THEN `time.Parse`, not `time.Parse` alone.**
   `time.Parse("2006-01-02", …)` accepts `2026-1-5`, so `#241`'s validator admits
   single-digit months and days and renders a date back that the caller never
   wrote. `campaignDateRE` refuses it first.

The branch version also carries `flightWindowStep`, which names Google's defaults
("starts immediately", "no end date") in the campaign-created step log rather than
leaving a reader to know them.

**What was taken from `#241` rather than discarded.** Its comments recorded two
facts the branch's did not, and both were folded into the surviving code:

- `applyCampaignConfig` writes these to the campaigns table's
  `start_date`/`end_date` columns. Before the config field existed Google passed
  `""` for both, so those columns stayed NULL for every Google campaign and the
  settings readback had nothing to compare upstream dates against. Microsoft still
  passes `""`, so Google is not the last adapter without a window — only the one
  whose readback made the absence visible.
- The settings readback's nil-recorded-side rule needed narrowing. Adoption is NOT
  "a campaign this service never configured": it persists whatever window the
  ADOPTING request supplied, so the recorded side reflects that request and not
  whatever created the campaign upstream — a disagreement the comparison is right
  to surface rather than excuse.

**Docs.** `internal-platform-googleads.md`, `internal-dispatch.md` and
`docs/api-catalog.md` all auto-merged cleanly and therefore ended up stating the
same feature twice, once per PR, with the duplicate half naming a `toGoogleDateTime`
helper that no longer exists. Each was collapsed to one account, keeping whichever
sentences were true of the surviving code. `docs/api-catalog.md` is the
consumer-facing validation contract for `CreateCampaigns.config` (typed `Any` in
`design/`), so a stale second spelling there is a contract defect, not untidiness.

**Tests.** Main's seven flight-window tests auto-merged and were written against
the pointer signature. Six were kept and now run against the surviving validator —
they cover the end-to-end payload shape on both channels and the
refused-before-any-mutate property, which is real coverage this branch's unit tests
do not duplicate. `TestValidateFlightWindow_AcceptsASingleDayAndAnOpenEnd` was
dropped as an exact duplicate of the same-day and open-end subtests already in
`serving_readiness_test.go`, and one error-text assertion was retargeted at the
surviving message, which carries the repo's `google-ads` package prefix.
