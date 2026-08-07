# 2026-08-05 — LFXV2-2023 GA-3c: definite-4xx coverage for the PAUSE partial cascade

**Update** — Closed a suppressed Copilot finding on PR #68: the PAUSE-path partial-cascade wrap
(campaign succeeds, then `UpdateAdGroupAndAdStatus` fails) only had a 5xx regression test
(`TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure`), not a definite-4xx one — the more
important case, since a definite 4xx is what `wrapUnconfirmed`'s ambiguity classification would
otherwise let through as a clean error. Added
`TestGoogleAds_ToggleStatus_PauseCascadeChildDefiniteFailureIsUnconfirmed`, which fails
`adGroups:mutate` with a definite 400 after the campaign pause succeeds and asserts the returned
error is `Unconfirmed() == true`. The ACTIVATE-cascade docs (`internal-platform-googleads.md`,
`internal-dispatch.md`, and the `ToggleStatus` function comment) were checked and already
correctly describe ACTIVATE as refused in GA-3c and re-enabled in GA-4 — no doc changes needed.
