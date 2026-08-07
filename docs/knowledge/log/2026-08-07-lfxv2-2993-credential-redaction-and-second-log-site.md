# 2026-08-07 — The reflected credential, and the log site the last fragment missed

**Update** — Closed the live review findings on PR #72 (`internal/platform/meta/client.go`,
`internal/service/brief.go`, and their tests).

**Bounding is not redacting.** `safeErrSummary` caps an error at `errSummaryMaxRunes` and
replaces non-graphic runes with U+FFFD. Neither operation removes a secret: a token is
printable and short. The one place a credential can actually be dropped is where the
untrusted text ENTERS the error chain, which for this client is the non-Graph fallback in
`doRequest` — reached exactly when the error body is NOT a Graph envelope, i.e. a proxy,
CDN, or WAF page. Any upstream that echoes the request echoes a live credential, and the
fallback then stored it in `APIError.Message`, from which every log line and every 5xx
inherits it.

**Correction (review pass):** an earlier draft of this fragment, and the comment at the
guard, said this client authenticates with `access_token` (and `appsecret_proof`) **in the
query string**. It does not — `doRequest` sets an `Authorization: Bearer` header and never
appends the token to the query. The guard is still needed, for a reason the wrong
description obscured: a reflection that echoes request HEADERS echoes the Bearer token,
which is why `redactCredentials` handles `Bearer <token>` and not only `key=value`. The
query-string form does still show up — in bodies echoing a Meta-constructed `paging.next`
URL, which genuinely carries the token as a parameter — so both shapes are covered. A
security comment that names the wrong credential path is worse than none: it invites the
next reader to "simplify" the guard down to the path that was never used.

`redactCredentials` replaces the VALUE and keeps the KEY — that the body echoed an
`access_token`, or a `Bearer` scheme, is itself the diagnostic — before truncation.

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
