-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- One live row per LinkedIn campaign: stop one upstream LinkedIn campaign from being bound to
-- two briefs (in any project) once LinkedIn joined adoption (LFXV2-2665).
--
-- A SEPARATE index, not a wider 000020 predicate — as 000020 and the LFXV2-3056 log entry
-- require: whether a global key is right is a per-provider question, and 000020's comment
-- argues from Google Ads' shared customer, which is not LinkedIn's reason.
--
-- LinkedIn's reason: a campaign id is GLOBALLY unique, not account-scoped. LinkedIn documents
-- URNs as "Globally Unique Identifiers (GUID)" (https://learn.microsoft.com/en-us/linkedin/shared/api-guide/concepts/urns),
-- and a campaign's URN is urn:li:sponsoredCampaign:{id} — the id alone, with no account
-- component — which is why the Ad Analytics finder addresses campaigns by that URN with no
-- account beside it. So two rows holding the same LinkedIn id ARE the same paid campaign,
-- whichever project or connection wrote them, and keying on (platform, platform_campaign_id)
-- without the account is exact: it can neither false-reject (no two accounts share an id)
-- nor false-accept.
--
-- Without it, adoption's ON CONFLICT arbitrates only the DESTINATION slot, so the same
-- LinkedIn campaign could be adopted into two briefs and both rows would pause or delete one
-- live paid campaign against each other.
--
-- AdoptCampaign classifies a violation of this index exactly as it does 000020's:
-- ErrPlatformCampaignAlreadyBound (409). A dispatch that resolves to a LinkedIn campaign
-- already bound elsewhere (the create path reuses a same-name campaign in its group) gets the
-- same sentinel from UpsertCampaign, and the claim is retained for reconciliation.
--
-- platform_campaign_id IS NOT NULL and status <> 'deleted' for 000020's reasons (claims
-- before an id exists are not bindings; soft delete frees the campaign).
--
-- CONCURRENTLY, one statement per file, no transaction, and NO `IF NOT EXISTS` — all for the
-- reasons spelled out in 000020: a retry over an INVALID index left by a failed CONCURRENTLY
-- build must fail with 42P07, not record this version clean over an index that enforces
-- nothing. A deploy whose data already holds two live rows for one LinkedIn id fails the build
-- loudly; resolve the duplicate binding, drop the INVALID index, and re-run.
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_linkedin_campaign_live
    ON campaigns (platform, platform_campaign_id)
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'linkedin-ads';
