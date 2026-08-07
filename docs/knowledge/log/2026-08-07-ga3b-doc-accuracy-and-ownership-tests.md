# 2026-08-07 — LFXV2-2023 GA-3b documentation accuracy and ownership coverage

**Update** — Corrections to claims made by the GA-3b slice, plus the test coverage
its account-ownership guard was missing.

**UTM handling was documented backwards.** `docs/knowledge/code/internal-platform-googleads.md`
and the `2026-08-04-ga3b-adgroup-ad-cascade.md` entry both said every pre-existing
`utm_*` key on the registration URL is preserved. `buildAdFinalURL` sets `utm_source=google`
and `utm_medium=cpc` UNCONDITIONALLY (`q.Set`) — deliberately, so a click on a Google
CPC ad is never attributed to an earlier channel's tag — and uses `setIfAbsent` only for
`utm_campaign` and `utm_content`. The concept now states the split; this entry supersedes
the claim in the 2026-08-04 entry, which is left in place per the one-file-per-entry rule.

**`validateResourceKind`'s comment named two symbols that do not exist**
(`compositeResourceID`, `adGroupCriterionID`). `adGroupAdID` is the only composite
splitter in the package.

**`CreateCampaign`'s exported comment described the old budget→campaign flow**, while the
function now runs a budget→campaign→ad-group→ad cascade with partial-result semantics past
the campaign stage. A caller reading the comment immediately above the method saw a stale
contract.

**The account-ownership branch had no test.** `TestValidateResourceKind_WrongCustomerRejected`
now exercises it for all three kinds that reach it (campaigns, adGroups, adGroupAds), each
paired with the same shape under the correct customer so a failure means the ownership check
specifically, not an unrelated rejection. Deleting the `pathParts[1] != c.account.CustomerID`
comparison fails all three subtests; before this, it left the suite green while the client
persisted ids belonging to another Google Ads account.
