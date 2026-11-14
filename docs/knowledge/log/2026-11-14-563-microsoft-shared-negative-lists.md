# 2026-11-14 — #563 Microsoft shared negative keyword lists at create

**Update** — Gap 9 of the Microsoft Advertising gap review (#563) adds
`microsoftConfig.sharedNegativeKeywordListIds`: existing account-level negative keyword lists
associated with the campaign a create confirms.

- `microsoft.ValidateSharedNegativeKeywordLists`: at most 20 ids, each a canonical positive int64,
  no repeat.
- Before anything is created, one `GetSharedEntities` read must hold every id as a
  `NegativeKeywordList`; otherwise the job fails with nothing created.
- After the campaign is confirmed, one `SetSharedEntityAssociations` call associates them. The
  per-list outcome is recorded as `sharedNegativeKeywordLists`; any FAILED or UNCONFIRMED list
  leaves the campaign `created_degraded` with a step, never failing the job.
- Gated on `global_marketing_ops` (`service.MicrosoftMarketingOpsInputs` and the dispatch
  field-classification test), because the lists belong to the shared LF account.
- A wrong type, a rule breach, or the list while `MICROSOFT_SHARED_NEGATIVE_LISTS_ENABLED` is off is
  a synchronous 400, re-checked in Dispatch. The chart sets the flag to `"false"`.

Not verified live: no Microsoft call was made. The REST paths (`SharedEntities/Query`,
`SharedEntityAssociations/Set`) and shapes are from the Campaign Management v13 reference, and
Microsoft's answer for an association that already exists is unconfirmed.
