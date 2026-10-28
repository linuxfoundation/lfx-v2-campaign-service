# 2026-10-27 — Google Ads targeting lookups: review fixes (PR #411)

**Fix** — the user-list read is now refused with 409 (`ErrUserListsSharedTenancy`) on a
shared-tenancy platform, which today means Google Ads. Each project's own connection points at
the one shared customer, and no user list is recorded per project, so an account-wide read
exposed every foundation's audiences to any project `campaign_manager`. The refusal happens
before any credential is resolved. The geo search now caches answers for 6h, bounded at 2048
entries. Both lookup clients use caller-scoped token refresh. The user-list GAQL is bounded
(`ORDER BY user_list.id LIMIT 501`). `limit` no longer has a Goa `Default`, so the generated
client stops sending `limit=0`. See
[google-ads-targeting-lookups](../architecture/google-ads-targeting-lookups.md).
