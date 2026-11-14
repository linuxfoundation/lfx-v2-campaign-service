# 2026-11-14 — #563 Microsoft remarketing-list picker read

**Update** — The follow-up to gap 4 of the Microsoft Advertising gap review (#563) adds
`GET /projects/{project_id}/microsoft-ads/remarketing-lists` (`list-microsoft-ads-remarketing-lists`),
which lists the remarketing lists a create can send as `microsoftConfig.audienceIds` /
`excludedAudienceIds`.

- One `GetAudiencesByIds` call (`POST Audiences/QueryByIds`, no ids, `Type: "RemarketingList"`):
  the pre-create check's own call, so every listed id is one it accepts. Response `account_id`,
  `lists` (`id`, `name`, `description`, `scope`, optional `membership_duration`, optional
  `search_size`) sorted by name then id, `list_count`.
- An unbelievable answer (no array, a null entry, a bad or repeated id, another audience type,
  more than 5,000 entries, any partial error) is a 503 with nothing partial.
- Gated on `global_marketing_ops` through the gateway's `marketing-ops-api` rule, like the Reddit
  and X pickers: the shared LF account holds every foundation's lists. The HTTPRoute's
  `microsoft-ads/(...)` branch forwards the base path only; `parity_test.go` pins both.
- Behind `MICROSOFT_REMARKETING_LISTS_ENABLED` (chart `"false"`): off, the read is a 400 "not
  supported" before Microsoft is contacted.

Not verified live: no Microsoft call was made. That `AudienceIds: null` returns every list of the
type is from the Campaign Management v13 `GetAudiencesByIds` reference.
