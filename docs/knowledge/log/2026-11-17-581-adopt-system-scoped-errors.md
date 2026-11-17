# 2026-11-17 — #581: system-scope adopt lookup errors; align the 500/503 contract

**Fix** — `LinkedInDispatcher.LookupCampaign` with `account_id` reads through the LF system row, but
its `GetCampaignSettings` connection-defect branch (and the unusable-account-id refusal) returned a
bare `ErrConnectionNotUsable`, so an expired LF token or a rejected LF application credential
reached adopt-campaign as the 409 that tells the caller to repair the project's connection. Both
now go through `res.systemScoped`, as `linkedInLookupError` already did for the adoptable list:
`ErrSystemConnectionNotUsable`, the operator-owned 500. A no-op on the project's own row, so an
adoption without `account_id` keeps its 409. Pinned end to end (real dispatcher, LinkedIn 401,
`BriefService.AdoptCampaign`) by `TestLinkedIn_AdoptLookupSystemCredentialRejected`.

- `list-linkedin-ads-adoptable-campaigns`' DSL description advertised 503 for an expired system
  credential; the implementation (`linkedInLookupError` → `classifyDiscoveryError`) answers 500.
  The description and the api-catalog row now say 500 for an expired/unusable LF system credential
  and 503 for a transient failure or an untrustworthy page. adopt-campaign's description and
  catalog row name the same 500.
