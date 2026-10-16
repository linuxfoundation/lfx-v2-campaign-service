# 2026-10-16 — campaign `adopted` flag and the `campaign_adopted` resume refusal

**Update** — follow-up to lfx-self-serve #3741. The UI could not disable Resume on adopted
campaigns or say why a resume was refused: neither the campaign result type nor the indexed
`CampaignDoc` said a campaign was adopted, and the refusal was the generic reason-less 409 from
`ToggleCampaignStatus`'s `ErrCampaignNotProvisioned` arm. Two additive, backward-compatible
changes:

- **`adopted` on the campaign** (design `Campaign`, optional read-only Boolean; `CampaignDoc`,
  `omitempty`). `true` or ABSENT, never `false`. Derived from one marker, `"adopted": true` in the
  row's result blob, read by `model.CampaignAdopted`. `BriefService.AdoptCampaign` now stamps it
  on every platform via `model.MarkResultAdopted` (previously only LinkedIn's adapter wrote it;
  Google recorded only `customerId`, the others a prose Steps line, which is deliberately not
  parsed). LinkedIn's `linkedInAdopted` delegates to the same reader. Rows adopted before this
  change on platforms other than LinkedIn read absent.
- **`reason=campaign_adopted`** on toggle-campaign-status's 409 for a RESUME of an adopted row —
  added to the shared `ConflictError.reason` Enum. Decided from the row, only for `active`; every
  other not-provisioned 409 is unchanged and reason-less; pause of an adopted campaign still
  works. Fixed message, no upstream data.

Documented in `docs/api-catalog.md` (get-campaign, adopt and status-toggle rows, and the
`reason` paragraph), `code/internal-service.md` and `code/internal-infrastructure-indexer.md`.
Tests in `internal/service/campaign_adopted_test.go` and
`internal/domain/model/campaign_adopted_test.go`; seven mutants (stamp dropped, reason arm
dropped, ACTIVE condition dropped, index mapping dropped, absent-vs-false, marker value, null
blob) each fail a test.
