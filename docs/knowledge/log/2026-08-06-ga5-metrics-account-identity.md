# 2026-08-06 — GA-5: verify the campaign's ad account before reading its metrics

**Update** — Review found that `ReadMetrics` resolved the project's CURRENT Google Ads
connection while the campaign row stored only an account-scoped numeric campaign id. Those
are two different accounts whenever `UpdateGoogleAds` has re-pointed the connection since the
campaign was created — a re-auth against a different login, a recreated account, an operator
correcting a mis-entered customer id.

The failure mode is silence, not an error. Google Ads scopes `campaign.id` to the customer in
the request path, so querying a campaign id under the wrong customer returns an EMPTY result
set, which `GetCampaignMetrics` reports as zeros. A dashboard cannot distinguish that from a
campaign with genuinely zero activity. And because ids are only account-unique, a collision
returns another account's impressions, clicks and spend as if they were this campaign's.

`googleads.CampaignResult` now carries `CustomerID`, stamped from the closure every returned
result descends from — so ambiguous partials carry it too, which is exactly when knowing
which account to reconcile in matters most. `Client.CustomerID()` exposes the account the
resolved connection points at. `ReadMetrics` compares them BEFORE contacting Google and
returns `domain.ErrCampaignAccountMismatch`, mapped to 409 (state, not transport — a retry
now fails identically) with the two customer ids logged server-side rather than returned to
the client.

Rows created before the field existed fall back to the `ocid` query parameter of the stored
`googleAdsUrl`, which the create path builds from the same value. Where neither is present
the creating account is unknown; an unknown cannot PROVE a mismatch, so the read proceeds
rather than turning the guard into a wall in front of every existing row.

Verified binding: neutering the comparison fails
`TestGoogleAds_ReadMetrics_ForeignAccountIs409AndNeverQueries` on both the missing 409 and
the "must not be queried" assertion; removing the `ocid` fallback fails only the legacy
sub-case and `TestGoogleAdsCreationCustomerID/legacy_ocid_fallback`.

**Update** — Copilot then pointed out that the invariant was enforced on the READ side only:
`GoogleAdsDispatcher.ToggleStatus` resolved the same current connection and mutated the
stored campaign/ad-group/ad ids without ever comparing accounts. That is the sharper half of
the same defect. A read against the wrong customer returns another account's numbers; a
MUTATE against the wrong customer pauses a stranger's live campaign, or enables one nobody
asked to run — real spend, on resources this project does not own.

The same pre-flight comparison now sits in `ToggleStatus`, placed above the PAUSE/ACTIVATE
split so both paths are covered by construction rather than by two parallel checks that can
drift. It runs immediately after the client resolves and before any Google call, and returns
`domain.ErrCampaignAccountMismatch`.

`ToggleCampaignStatus`'s status mapping gained the matching case, ordered BEFORE the
`Unconfirmed()` branch. Both parts of that matter: 409 rather than 503 because a retry is
refused identically (state, not transport), and not-unconfirmed because the platform was
never contacted — reporting an unconfirmed outcome would send an operator reconciling a
mutation that provably did not happen. The two customer ids stay in the server-side log, as
on the metrics path.

`TestGoogleAds_ToggleStatus_ForeignAccountIs409AndNeverMutates` covers both recorded-id
shapes across both PAUSE and ACTIVATE (the ACTIVATE fixtures carry provisioned children and
keyword criteria so the earlier gates pass and this guard is what fires), and asserts the
non-unconfirmed classification alongside the 409. Verified binding by neutering the
comparison: all four sub-cases fail, and the test server records the `adGroups:mutate`,
`adGroupAds:mutate` and `campaigns:mutate` calls that would have gone to the wrong account.
`TestGoogleAds_ToggleStatus_UnknownOrMatchingAccountStillToggles` keeps the guard from
becoming a wall in front of rows that record no customer id, mirroring the read side.
