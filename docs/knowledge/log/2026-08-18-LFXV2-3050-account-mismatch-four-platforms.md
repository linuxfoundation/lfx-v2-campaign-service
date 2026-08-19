# 2026-08-18 — LFXV2-3050 the account-mismatch guard reaches the last four platforms

**Fix** — `ErrCampaignAccountMismatch` existed only on google-ads and microsoft. On linkedin,
meta, reddit and twitter a campaign created under account A was silently read, toggled, and
measured against account B. The system-account fallback (LFXV2-3040) makes that reachable:
create under the LF system account, connect a project account, then read.

**The recoverable-source question has a different answer per platform.** Microsoft's helper
reads an explicit `accountId` and falls back to the `aid=` param of a stored console URL, so
pre-existing rows stay checkable. LinkedIn and Meta have an equivalent — `linkedInUrl` carries
`/campaignmanager/accounts/<id>/campaigns` and `metaUrl` carries `?act=<digits>` — so both got
the two-source helper. **Reddit and Twitter have none**: their result URLs are the bare
ads-manager constants (`https://ads.reddit.com`, `https://ads.x.com`) and never carried an
account. Every row written before this change therefore records no provenance anywhere and is
waved through as "unknown"; only a re-dispatch can give it one. That is stated in the helper
doc comments rather than left for a reader to infer from an absent fallback.

**Meta needed a normalisation the others did not.** The connection stores `act_777` while
`metaUrl` carries the bare `777`. Comparing them raw reports every legacy row as a mismatch —
a false 409 on a campaign that is perfectly in scope. `normalizeMetaAccountID` puts both sides
in one vocabulary, and an empty input stays empty rather than becoming `"act_"`.

**Ordering is the defect that hides the defect.** Each of the four had a narrower guard that
would answer first and mask the mismatch: linkedin's creative-servability check (inside the
client call, so it also CONTACTS the platform), meta's ad-set check, reddit's child-id check,
twitter's line-item check. On a re-pointed connection all four describe a campaign in a
DIFFERENT account, so each would explain the wrong campaign. Provenance now runs first
everywhere, exactly as microsoft.go records at the same seam. Each ForeignAccount test carries
a probe case with the child id ABSENT, so a reordering fails rather than passing on the arm
where both guards agree.

**A guard test over a hand-written blob does not pin the create path.** Removing the stamping
from all three twitter result sites left every mismatch test green — the fixtures supply their
own blobs. On reddit and twitter that mutation is fatal, not cosmetic: with no URL fallback an
unstamped row is permanently "unknown" and the guard can never fire. Each platform now has a
`Dispatch...StampsCreatingAccount` test that drives a real create and reads the persisted blob
back through the guard's OWN helper, pinning reader and writer to one shape. The same mutation
on linkedin and meta correctly SURVIVES — their URL fallback still recovers the account — and
removing both sources together fails them, which is how the fallback was verified rather than
assumed.
