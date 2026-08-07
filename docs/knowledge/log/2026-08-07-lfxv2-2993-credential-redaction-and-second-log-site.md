# 2026-08-07 — The reflected credential, and the log site the last fragment missed

**Update** — Closed the live review findings on PR #72 (`internal/platform/meta/client.go`,
`internal/service/brief.go`, and their tests).

**Bounding is not redacting.** `safeErrSummary` caps an error at `errSummaryMaxRunes` and
replaces non-graphic runes with U+FFFD. Neither operation removes a secret: a token is
printable and short. The one place a credential can actually be dropped is where the
untrusted text ENTERS the error chain, which for this client is the non-Graph fallback in
`doRequest` — reached exactly when the error body is NOT a Graph envelope, i.e. a proxy,
CDN, or WAF page. That matters here more than it would for most clients, because Meta is
authenticated with `access_token` (and `appsecret_proof`) **in the query string**: any
upstream that echoes the request line echoes a live credential, and the fallback then
stored it in `APIError.Message`, from which every log line and every 5xx inherits it.
`redactCredentials` now replaces the VALUE and keeps the KEY — that the body echoed an
`access_token` is itself the diagnostic — before the snippet is truncated.

The two tests constrain each other on purpose. `TestNonGraphErrorBodySurfaces` pins that
the raw body is not dropped; `TestNonGraphErrorBodyRedactsReflectedCredentials` pins what
must be dropped from it. A "fix" that discards the snippet fails the first; one that keeps
it verbatim fails the second. The sentinel in the second is deliberately printable and
control-character-free, so `safeErrSummary` would pass it through untouched.

**`GetCampaignMetrics` has TWO failure-path logs, and only one was scrubbed.** The
2026-08-06 fragment said both call sites went through `safeErrSummary`. The default branch
did; the `ErrMetricsWindowUnsupported` branch still wrote `merr` verbatim — reachable with
the same kind of adapter-wrapped upstream text, so the unbounded body had an open path to
the sink through it. This is the second time on this branch that a fragment described
work more complete than the code, and the same reviewer class caught it both times; the
cheap defence is one test per call site, since a test on either alone passes while the
other logs verbatim. `TestGetCampaignMetrics_WindowUnsupportedErrorIsScrubbedBeforeLogging`
is that second test, revert-verified.
