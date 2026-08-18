# 2026-08-18 — LFXV2-3260 Microsoft metrics: CSV parser defects

**Fix** — Three defects in the Microsoft report CSV parser, each reproduced with a failing
probe before being fixed and each mutation-verified afterwards with a COMPILING change. All
three share one failure class: they turn a malformed or oversized report into a plausible
NUMBER rather than an error, and a number is what the dashboard renders as a measurement.

**An oversized CSV truncated into a partial total.** `parseReportZip` streamed the zip entry
through `io.LimitReader(rc, reportDownloadCap+1)` straight into `csv.NewReader`.
`io.LimitReader` reports EOF at its limit — it does not error — so `csv.ReadAll` accepted a
syntactically complete PREFIX and returned fewer rows, which folded into a total that looked
authoritative. The compressed side (`downloadReport`) already read `cap+1` and explicitly
errored above the cap; the decompressed side had no equivalent. Reproduced by aligning a
payload so byte `reportDownloadCap+1` fell exactly on a row boundary: 5000 rows vanished with
no error at all. A boundary that lands mid-quoted-field instead surfaces a parse error, which
is why the naive probe misses this and the aligned one is the real test. The decompressed
stream is now read to a buffer, size-checked, then parsed. Pinned by
`TestParseReportZip_OversizeCSVIsRefusedNotTruncated`, with
`TestParseReportZip_AtCapIsAccepted` holding the `+1` boundary so the guard rejects only what
genuinely exceeds the cap.

**`dropTrailerRows` silently discarded truncated DATA rows.** It dropped any row narrower
than the header, commented "A DATA row always carries the full column set" — an assumption
about response SHAPE on a contract this file declares UNVERIFIED. A real data row missing a
trailing field was dropped without error and its impressions/clicks/spend disappeared into a
clean-looking total. This is precisely the class the missing-column guard in `foldReportRows`
refuses by name, on the stated reasoning that "a zero that came from an absent column is
indistinguishable, to every consumer, from a measured zero". The trailer is now identified
POSITIVELY — blank, or a first cell beginning with `©` — and `width` is gone from the
signature. Confirmed before fixing that `parseReportInt`'s `col >= len(row)` error was
genuinely unreachable through `foldReportRows`, because `dropTrailerRows` removed exactly the
rows that would have tripped it; it is now live, and is what reports the short row. Pinned by
`TestGetCampaignMetrics_ShortDataRowIsAnErrorNotADrop` and
`TestDropTrailerRows_IdentifiesTrailerPositively`.

**The accumulated totals were never overflow-checked.** Per-row values were validated for
NaN/Inf/negative/range, but `out.Impressions += imp`, `out.Clicks += clk` and
`out.CostMicros += int64(scaled)` could each wrap int64 across many individually-valid rows,
producing a negative total. Added the three checked additions `reddit/metrics.go` already
uses. Note each guard needs TWO rows to trip — the total starts at zero, so a single row can
never exercise it, and a one-row test would sit permanently green. The table in
`TestFoldReportRows_TotalsAreOverflowChecked` uses two rows per case;
`TestFoldReportRows_MultiRowTotalsStillSum` guards against the checks being written so
tightly they reject ordinary multi-row reports.

**Mutations.** Six, each compiling, each caught: raising the decompressed cap 100x and
reverting to the streaming `LimitReader` both fail the oversize test with the same "5000 rows
were silently dropped" message; reintroducing the width filter fails both defect-2 tests;
and `&& false` on each of the three overflow conditions fails its own subtest with the
wrapped negative visible (`-2`, `-2`, `-7378697629483810816`).

**Note** — `submitReport` populates BOTH `Scope.AccountIds` and `Scope.Campaigns`, and
Microsoft's documentation states the report scope is a UNION of the two, which would make
this campaign-scoped read return account-wide totals. This entry does NOT change that
behavior — the finding is recorded in
`2026-08-18-LFXV2-3260-scope-union-finding.md` and needs a decision before any code moves.
