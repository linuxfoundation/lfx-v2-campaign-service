# 2026-10-28 — Meta: a wrong-typed identity field is a 400, not "absent"

**Fix** — `MetaDispatcher.PreflightCreate` decodes `metaConfig` best-effort and ignores type
errors. So `"instagramUserId": true`, or `"dsaBeneficiary": 1` on an EU target, decoded to `""`
and passed the publish-identity check as absent. The request got a 202, and the job then failed
on `Dispatch`'s strict decode (PR #414 review). `checkMetaIdentityShape` now runs first and
refuses any of `instagramUserId`, `dsaBeneficiary` and `dsaPayor` that is present with a
non-string JSON type: a 400, `"<key>" must be a string`. Keys are matched case-insensitively, as
encoding/json matches them, and `null` still reads as absent. Covered by new cases in
`TestMeta_PreflightCreate_PublishIdentity`. See
[internal/dispatch](../code/internal-dispatch.md).
