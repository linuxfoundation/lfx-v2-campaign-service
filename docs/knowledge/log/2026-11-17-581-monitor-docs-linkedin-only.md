# 2026-11-17 — #581: the unrecorded-campaign monitor rule is LinkedIn's only

**Docs** — The rule that keeps a campaign recording no creation account on the project's own
connection, and omits (logs) it on the LF system credential, is implemented only by
`LinkedInDispatcher.MonitorScoperForConnection` / `linkedInMonitorScope` (#628). Google Ads and X
still refuse such a campaign through `requireRecordedMonitorProvenance` (409).

- design/connection.go: the sentence stays on `monitor-linkedin-ads-account`; the
  `monitor-google-ads-account` and `monitor-twitter-ads-account` descriptions are back to their
  main wording ("or recording none, fails the read with 409"). Regenerated with `make apigen`.
- architecture.md § Account Tenancy: LinkedIn no longer reads as failing its monitor like Google
  Ads; the paragraph states its rule (kept on its own connection, omitted and logged on the system
  credential, a resolution failure is an error, omitting `account_id` with no recorded account is
  still a 409). Google Ads, Reddit and X are unchanged.
- api-catalog.md and docs/knowledge already scoped the rule to LinkedIn; nothing else changed.
