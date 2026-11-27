# 2026-11-27 — #666 LinkedIn: no recorded staff exclusions for a campaign found by name

**Fix** — PR #688 review (Copilot). A profile create can reuse a same-name campaign without
touching its targeting, yet the row recorded the request's `employerExclusions` as the original
list, so the targeting update lever could re-merge a list the campaign never had and drop the ones
it really has. `CampaignResult.CampaignCreated` (not persisted) is set on every result built after
the campaign exists, partial ones included, and is false for a campaign found by name;
`campaignFromLinkedIn` records `EmployerExclusions` only when it is true. A reused campaign's row
records none, and the lever refuses it 409. Pinned by
`TestCampaignFromLinkedIn_ReusedCampaignRecordsNoEmployerExclusions` and the `CampaignCreated`
assertions in the bidding and partial-variant create tests.
