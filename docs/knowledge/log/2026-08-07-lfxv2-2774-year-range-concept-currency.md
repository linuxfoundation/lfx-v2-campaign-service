# 2026-08-07 — LFXV2-2774: the snowflake concept still described the old four-digit rule

**Update** — `ResolvePastEventNames` narrowed `currentYear` from "any 4-digit year" to a
4-digit **19xx or 20xx** year. `docs/knowledge/code/internal-platform-snowflake.md` still
documented the old, wider rule, so the concept and the code disagreed about the contract of
a required parameter.

**Fix** — The concept now states the range and, more usefully, WHY it is a rejection rather
than a comment: `yearInName` can only extract a 19xx/20xx year, so a four-digit `currentYear`
outside that range does not fail — it degenerates. Above the range (`"9999"`) every extracted
year is strictly below it, the `extractedYear >= currentYear` exclusion never fires, and
future editions come back labelled as past ones. Below it (`"0202"`) every row is excluded
and the caller gets a silently empty result. Two wrong answers with no error attached, which
is exactly the class of bug a boundary rejection exists to prevent.

**Note** — CLAUDE.md requires the concept update alongside the dated log. The PR's earlier
revisions carried only the log, which is what this entry corrects; the concept edit above is
the correction, so the shipped PR has both. The two are not interchangeable: the log records
that a thing changed, the concept is what the next reader consults instead of the source, and
a log entry alone leaves the concept quietly asserting the superseded contract.
