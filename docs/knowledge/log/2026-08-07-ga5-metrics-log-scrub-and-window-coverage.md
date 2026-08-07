# 2026-08-07 — GA-5 metrics: a decode error is log input, and an untested mapping is a wrong date range (LFXV2-2023)

**Update** — `GetCampaignMetrics` no longer echoes unparseable metric values into its error,
and `WindowFor`'s seven branches are pinned to their exact GAQL literals.

Both findings are the same shape: a value that looks like debugging detail is actually part of
a contract with something downstream.

**The decode error was reaching a log sink.** When a metric field fails to parse, the value
that failed came verbatim from the upstream response body — and the service's default failure
branch renders the platform error into a warning log. Interpolating it meant an attacker-
influenceable string (newlines included) landing in the log stream, from a code path whose
whole job is to report that the string was malformed. The error now names which of
`impressions` / `clicks` / `costMicros` failed and nothing else, which is the part a responder
can act on. The general rule: before putting an upstream value in an error, ask where that
error is rendered.

**Six of seven window translations were untested.** Coverage exercised `last_30_days` and the
invalid-input path, so a wrong literal in any other branch would compile, pass the
`validMetricsWindows` injection allow-list — because it is still a *valid* literal, just the
wrong one — and query a different reporting period. The failure has no visible symptom: the
call succeeds and returns plausible numbers for dates nobody asked for. That combination
(compiles, passes the guard, silently wrong) is what makes a translation table worth a
table-driven test even when each branch is a one-liner. The test spells the GAQL strings out
literally rather than comparing the constants to themselves, and size-checks the table against
the allow-list so a window added without a mapping fails here.
