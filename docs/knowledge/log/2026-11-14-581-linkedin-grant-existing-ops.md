# 2026-11-14 — #581 LinkedIn levers reach campaigns in granted accounts

**Update** — Step 2 of #581, shipped before any create path can use a grant ("never create what
you cannot pause"). Pause/resume, metrics, budget, bid and settings readback on a LinkedIn
campaign recorded in one of the system row's granted accounts now resolve the system credential
scoped to that grant (account and org taken from the grant). An account that is neither the system
row's default nor granted is refused as before, with nothing sent.

- `resolved.grants` / `grantAccount`, `resolved.grantScoped`, `existingOnSystem` (dispatch).
- `resolveExisting`'s system arms and `systemCreated` count grants.
- `cachedLinkedInClient` keys on the granted account.
- Tests: `TestLinkedIn_PauseReachesACampaignInAGrantedAccount`,
  `TestLinkedIn_GrantedAndDefaultAccountsDoNotShareAClient`, `TestGrantScopedHonoursOnlyTheSystemRow`,
  `TestLinkedIn_BrokenSystemRowIsBlamedForAGrantedCampaign`. Each fails when its rule is removed
  (cache key, grant lookup in `existingOnSystem`, grant attach on a cache hit, system-only scoping,
  the providerConfig copy, `systemCreated`'s grant check).
