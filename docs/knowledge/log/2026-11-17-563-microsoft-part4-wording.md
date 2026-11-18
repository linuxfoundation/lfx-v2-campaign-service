# 2026-11-17 — #563 part 4: exact flag-off errors and role fields in the Microsoft contract

**Docs** — Two review findings on #631 corrected in the `config` attribute description in
`design/brief.go` (regenerated OpenAPI and CLI follow). No behaviour changes.

- `searchPartners`, `adSchedules` and the device, age and gender bid adjustments are refused by a
  flag-off check whose message is `invalid campaign config: microsoftConfig: <field> disabled on
  this deployment ...` (for bid adjustments `<field>` is the comma-joined list of the ones sent),
  not the `microsoftConfig.<field>: ...` prefix their invalid-value refusals use. Each section now
  quotes the two messages separately. The other sections added in part 4 were already accurate:
  their flag-off messages also start `microsoftConfig.<field>: ...` (`microsoftConfig.bidding: ...`
  for a disabled conversion bidding strategy).
- The `adGroups` section said "the lists also need `global_marketing_ops`" right after
  `negativeKeywords`, implying per-group negative keywords are role-gated. It now names the only
  per-group fields `service.MicrosoftMarketingOpsInputs` checks: `audienceIds` and
  `excludedAudienceIds`.
