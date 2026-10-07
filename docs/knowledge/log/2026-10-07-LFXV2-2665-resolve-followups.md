# 2026-10-07 — LFXV2-2665 review follow-ups to the Meta/Reddit/X campaign-ref change

**Fix** — Four follow-ups from the review of #278:

- **Decoder 400s no longer echo the rejected value.** Verified empirically first: a malformed
  `platform_campaign_id` sent to any of the five `GET .../{platform}/campaign-ref` routes was
  answered by Goa's generated decoder — before `platformCampaignIDRule` could run — with a 400
  whose message carried the raw input (`... but got value "zz<script>..."`), because `buildMux`
  passed a nil formatter. Every generated server is now built with `nonEchoingErrorFormatter`
  (`cmd/campaign-service/error_formatter.go`), which rewrites the value-echoing Goa validation
  errors (`invalid_pattern`, `invalid_length`, `invalid_range`, `invalid_enum_value`,
  `invalid_format`, `invalid_field_type`) to a fixed sentence naming the field, never the value,
  part by part for a merged error. Status, `name`, `id` and flags are unchanged; only the
  `message` text of those errors changes, server-wide. `validation_echo_test.go` drives a
  marker-bearing id through the real mux to all five routes; it fails with the formatter unwired.
- **Reddit account item bound is now tested.** `TestListAdAccounts_AccountItemBoundIsReadPlusOne`
  spreads exactly 2,000 and 2,001 unique accounts over two businesses of two pages each, with one
  cross-business repeat that must not count: 2,000 is an answer, 2,001 is `ErrDiscoveryMalformed`
  with no partial list.
- **`docs/api-catalog.md`'s account-bootstrap discussion** still said Reddit "lacks the first
  half" with no discovery endpoint and no `ListAdAccounts`. It now says Reddit has both halves
  since LFXV2-2665 and is still excluded from `accountDiscoveryProviders` because its HTTP
  connection config Requires `account_id`. This corrects the 2026-10-07 resolve-and-reddit-accounts
  entry's claim that all such prose had been fixed.
- **The design comment above `list-google-ads-accounts`** said Reddit has no `ListAdAccounts` and a
  hand-entered account id; it now names `list-reddit-ads-accounts`. Comment only; `make apigen`
  produced no generated change. No other stale "Reddit has no discovery" claim remains outside
  the dated log entries.

Concepts updated: [cmd/campaign-service](../code/cmd-campaign-service.md),
[internal/service](../code/internal-service.md).
