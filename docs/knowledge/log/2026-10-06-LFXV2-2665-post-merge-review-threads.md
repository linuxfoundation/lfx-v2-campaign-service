# 2026-10-06 — Post-merge review threads (#254, #262–#265, #267, #270, #274)

**Fix** — review threads left unresolved on already-merged PRs, triaged against `main` and
fixed in one commit. Threads already fixed by a later PR (#251's by #253, #261's by #265) need
nothing here.

- **#254 — monitor 409 body.** `monitor-twitter-ads-account` declares its own
  `AccountMonitorConflictError` (`reason` required, exactly `account_too_many_active_campaigns` /
  `account_timezone_unsupported`) instead of the shared `ConflictError`, whose `already_exists`
  example advertised a response the method can never return. The two monitor reasons left the
  shared enum.
- **#262 — X budget echo.** A 2xx echo with a PRESENT `null` `daily_budget_amount_local_micro`
  is UNCONFIRMED; an omitted field is still accepted.
- **#263 — Microsoft keyword read.** The response `window` uses the Microsoft five-value enum.
  `KeywordReportReader.KeywordReportEnabled` checks the rollout gate and window before the
  empty-scope success, so a gated-off read is 400 whatever the project's campaigns are.
- **#264 — Reddit bid guard.** An absent or null current `bid_value` is refused
  (`ErrBidUnwritable`) before the PATCH, like an unparseable one.
- **#265 — Microsoft keyword levers.** `"Keywords": null` is an unanswered read, not an empty ad
  group; a non-empty `PartialErrors` naming no rejection (`[null]`, `[{}]`) is UNCONFIRMED. A
  credential-decrypt failure on the negative-keyword, keyword-action and keyword-targeting paths
  is logged against the failing row (`credentialOwnerProject`, honouring
  `ErrSystemConnectionOrigin`) with `requested_by_project_id`. `NegativeKeywords` carries a
  type-level example whose `applied_count` matches its `results`.
- **#267 — config_snapshot redaction.** `SnapshotText` reduces any `scheme://` run, not only
  http(s) (`file:///…` with no host is dropped) and `SnapshotURL` fails closed on one that will
  not reduce; the bracketed-IPv6 branch consumes userinfo before `[`; the `user:password@`
  username class admits RFC 3986 sub-delims (kept in step with the X screen). Key-collision
  suffixes are tracked per redacted base, so N colliding keys are linear work.
- **#270 — Meta `/adimages`.** The upload's non-2xx parse goes through `copyEnvelope`, and its
  over-cap abort keeps `ErrorSubcode` and `blameFields`.
- **#274 — keyword targeting.** `KeywordTargetingRemovals` carries a correlated type-level
  example. Reddit's whole-targeting PATCH is sent once (a 429 is UNCONFIRMED, never retried from
  the stale pre-read). X's `DeleteTargetingCriterion` maps a request `ProbeNotSent` proves never
  left to `ErrWriteNotSent` (`NOT_SENT`) before classifying API responses.
