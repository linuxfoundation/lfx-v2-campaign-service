# 2026-08-07 — Four suppressed findings, all real, all about a number that lies

**Update** — Closed the five suppressed Copilot findings on PR #72
(`internal/platform/meta/metrics.go`, `client.go`, `docs/api-catalog.md`, and the
2026-08-07 credential-redaction fragment).

These were never posted as threads — they sit inside a `<details>Suppressed comments</details>`
block in the review body, so the PR reported zero unresolved comments while carrying
four live defects. Three share a failure mode: **the reader turned malformed upstream
data into a confident number.** That is worse than an error, because a caller cannot
tell the difference.

- **`{}` read as "no delivery".** `Data` was a plain slice, so an absent or `null`
  `data` field decoded identically to `{"data":[]}` — both length zero. The second is
  Meta stating the campaign had no delivery; the first is a malformed 2xx that states
  nothing. Collapsed, a broken response published "0 impressions, 0 clicks, $0 spend"
  for a campaign that may be spending. `Data` is now `*[]struct{...}`, exactly as the
  ad-discovery path in `client.go` already does for the same shape — the precedent was
  in the same file.
- **Negative counters accepted.** `ParseInt` takes `-5` happily. Negative impressions
  or clicks produce a negative CTR in the public response, a value no consumer
  validates because it cannot legitimately occur.
- **Negative spend accepted.** The existing guard checked NaN/Inf; finite is not
  enough. A negative `CostMicros` is absorbed as a credit by every cost-per-click,
  pacing, and roll-up computation downstream.

The LinkedIn and Reddit readers already rejected both negative shapes. Meta was the
outlier, which is the kind of gap a per-platform review pass misses and a
cross-platform one catches.

**The fourth was a security comment naming the wrong credential path.** The guard at
the non-Graph error fallback said "this client authenticates by putting `access_token`
(and `appsecret_proof`) in the query string". It does not — `doRequest` sets an
`Authorization: Bearer` header and never appends the token to the query. The guard is
still needed, for the reason the wrong description hid: a reflection that echoes request
HEADERS echoes the Bearer token, which is why `redactCredentials` handles
`Bearer <token>` as well as `key=value`. The query-string form does appear, in bodies
echoing a Meta-constructed `paging.next` URL. Both are covered; the comment and the
knowledge fragment now say so. A security comment that names a path the code never uses
invites the next reader to simplify the guard down to it.

**Fifth: the catalog's platform-support table still listed only X, LinkedIn and Reddit**
while this PR makes Meta supported — so the capability would have shipped undiscoverable.
Meta's row is in, with the `date_preset` mapping noted.

All three metrics guards revert-verified individually. The empty-window test
(`_NoActivityInWindowReturnsZeroValue`) is the deliberate counterweight to the new
`_MissingDataFieldIsNotZeroActivity`: a fix that rejects both shapes fails it.
