# 2026-08-19 — the metaConfig → CampaignInput mapping had no test

**Fix** — Three tests on the forced-system-account branch could not fail, all found by
mutation before the PR opened.

**The dispatcher's Instagram/DSA mapping was untested at any layer.** Deleting
`InstagramUserID: cfg.InstagramUserID` from the struct literal in `internal/dispatch/meta.go`
left the entire `internal/dispatch` package green, as did transposing `DSABeneficiary` and
`DSAPayor`. The client-level tests in `internal/platform/meta` assert the wire payload *given*
a `CampaignInput`, so they start BELOW the seam that would break. Either mutation ships an ad
that is created, spends nothing, and sits unpublishable in Ads Manager with "Please add
Instagram account" — the exact failure the branch exists to remove.

`TestMeta_ConfigFieldsReachTheWire` now drives `Dispatch` through an `httptest` server and
asserts the request bodies. It deliberately does NOT rebuild a `CampaignInput` from the same
config: a test that re-derives the mapping asserts `x == x` and passes against a `meta.go`
that dropped the field. The input is envelope-shaped real JSON (`{"metaConfig": {...}}`), so
the `json` tags — the public request contract in `docs/api-catalog.md` — are pinned too.

**The two DSA disclosures held the same literal.** With both set to `"The Linux Foundation"`,
transposing them in `client.go` passed. Under the DSA the beneficiary and the payer are
legally distinct roles and are routinely different entities (an LF project as beneficiary,
LF Projects as payer), so a transposition is a real defect that reaches a regulated ad set.
The fixture now uses distinct values and cross-checks that they differ.

**The `SystemProjectID` short-circuit was unbound.** Removing
`&& projectID != model.SystemProjectID` from the forced-path guard kept every test green,
because both paths issue exactly one `Get(SystemProjectID, provider)` — so the test's
`len(repo.gets) != 1` assertion is identical on both sides. `fromSystem` is the only
observable difference, and it is load-bearing rather than bookkeeping: `resolved.systemScoped`
keys on it, so a system-scope request that took the forced path would gain a spurious
`ErrSystemConnectionNotUsable` and be classified 500/page-an-operator.

All four mutations were verified to COMPILE before being run — a build break proves nothing —
and each source file was diffed back to byte-identical afterwards.
