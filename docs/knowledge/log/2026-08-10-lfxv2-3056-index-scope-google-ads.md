# 2026-08-10 — LFXV2-3056: scope the adoption binding index to google-ads

**Fix** — a global uniqueness guard built for Google Ads would have false-rejected legitimate
Microsoft dispatches.

Review finding on PR #110 (Copilot, unresolved thread): migration `000020`'s
`uq_campaigns_platform_campaign_live` was keyed `(platform, platform_campaign_id)` over every
live row, for every provider. The index exists to enforce one service-owned invariant — one
upstream campaign binds to one brief — and the argument for keying it GLOBALLY rather than per
project rests entirely on a fact about Google Ads: it is one shared customer id across every
foundation (`docs/channel-connections-schema.md`), with one connection row per project pointing
at the same account. Two projects there really are the same account, so project-scoping would
let two briefs bind one live paid campaign and toggle it against each other.

That premise does not generalise, and the index was applying to providers where it is false.

## Why Microsoft breaks it

Microsoft campaign ids are ACCOUNT-scoped, not globally unique, and this service supports
separate per-project Microsoft connections — each account mints its own id space. Under the
unscoped index, account B minting an id that account A had already minted raises 23505 on a
perfectly ordinary dispatch. Two things make that worse than a merely noisy error:

- Only `AdoptCampaign` classifies 23505 as `domain.ErrPlatformCampaignAlreadyBound`. On the
  dispatch path the same violation surfaces as a generic 409, so the operator gets a conflict
  that names nothing useful.
- The dispatch is left holding an UNCONFIRMED partial until someone intervenes. Normal campaign
  creation is blocked for a collision that is not a collision.

The failure asymmetry that justifies the global key for Google Ads therefore inverts for
Microsoft: there, the "false reject" is not a rare theoretical case someone reads and fixes,
it is the expected outcome of routine use.

## The change

`1ca63e97` adds `AND platform = 'google-ads'` to the migration's `WHERE` clause:

```sql
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_platform_campaign_live
    ON campaigns (platform, platform_campaign_id)
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'google-ads';
```

Adoption is implemented only for Google Ads today, so the predicate costs nothing in coverage
and keeps every other provider's dispatch off the index entirely.

**When adoption gains a second provider, add that provider's uniqueness handling as a separate
constraint rather than widening this predicate.** Whether a global key is even correct there is
a per-provider question — it depends on whether that provider's campaign ids are account-scoped
— and answering it by extending an index whose comment argues from Google Ads' shared-customer
model would silently reuse a rationale that does not apply.

## Regression guard

`TestMigration000020_ScopesTheIndexToGoogleAds` (`outbox_repo_test.go`, beside the existing
`TestMigration000020_HasNoIfNotExists`) asserts the migration text carries the literal
`platform = 'google-ads'` — it needs no database, so the scope stays pinned even where the live
suite is skipped. The live-db
`TestLiveOneUpstreamCampaignBindsToOneBrief` gains a `microsoft is not constrained` sub-test
that inserts the same `platform_campaign_id` under two projects on `microsoft-ads` and requires
both to succeed. Before this fix the second insert raised 23505; the sibling google-ads
sub-test still requires that it does.
