# 2026-11-07 — #563 Microsoft create-time negative keywords

**Update** — `microsoftConfig.negativeKeywords` (gap 2 of the Microsoft Ads gap review, #563)
attaches campaign-level negative keywords right after a Microsoft Advertising campaign is
created. An LF event Search campaign needs them from the first impression ("jobs", "free",
"salary"); before this they could only be added afterwards with `add-negative-keywords`.

- Same client call as the lever, `AddCampaignNegativeKeywords` (`POST EntityNegativeKeywords`,
  `EntityType: "Campaign"`), so the same rules: 1–60 entries, Exact or Phrase in any casing
  (Broad refused), at most 100 characters of letters, digits, spaces and `& ' - .`, no repeated
  (text, match type) pair.
- Synchronous 400 (`invalid campaign config: microsoftConfig.negativeKeywords: ...`) from
  `MicrosoftDispatcher.PreflightCreate` for a rule breach or a value that is not an array of
  `{text, matchType}` objects, checked under any key casing and on the best-effort decode.
  Dispatch re-checks before any Microsoft call.
- Applied only to a confirmed campaign. Outcomes are recorded positionally on the result
  (`negativeKeywords`). A FAILED or UNCONFIRMED keyword, or a failed call, degrades the campaign
  (`created_degraded`) with a step pointing at `add-negative-keywords`; it never fails the job.
  Repeating the create does not retry them (a created or degraded campaign is returned as it is);
  only a dispatch that reuses an unconfirmed campaign by name re-sends them, and gets
  `ALREADY_PRESENT` for the ones already there.
- `NegativeKeywordOutcome` gained camelCase JSON tags for that record.
- `ValidateNegativeKeywords` now renders an unsupported match type through `domain.CallerValue`
  instead of `%q`, because its sentence reaches a persisted job error verbatim; this also applies
  to the `add-negative-keywords` lever.
- Default-off deployment flag `MICROSOFT_CREATE_NEGATIVE_KEYWORDS_ENABLED`; the chart sets
  `"false"`. Not role-gated: a negative keyword names no account resource.

Not verified live: no Microsoft call was made. Turn the flag on only after a live create
confirms the add against a just-created PAUSED campaign.
