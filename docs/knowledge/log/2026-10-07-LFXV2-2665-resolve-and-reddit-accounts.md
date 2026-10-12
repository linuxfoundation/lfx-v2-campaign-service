# 2026-10-07 — LFXV2-2665 Meta/Reddit/X campaign refs and Reddit account discovery

**Update** — Two capabilities, both `campaign_manager`:

- **Campaign-ref lookups for Meta, Reddit and X.** `GET /projects/{projectId}/{meta-ads,reddit-ads,twitter-ads}/campaign-ref`
  (`resolve-meta-ads-campaign`, `resolve-reddit-ads-campaign`, `resolve-twitter-ads-campaign`),
  the twins of `microsoft-ads/campaign-ref`: same payload shape, `platform-campaign-resolution`
  result, errors and semantics (DB-only, project-scoped, unowned id = 200 with empty `matches`,
  system scope 404, storage fault 500, unwired backend 503, every match returned). All five
  routes now share `resolvePlatformCampaignRef`, which takes the id rule as a parameter; Google
  and Microsoft pass the unchanged `validatePlatformCampaignID`, so their behaviour and tests are
  unchanged. The new routes use new `ValidateCampaignID` helpers in `internal/platform/{meta,reddit,twitter}`
  (Meta: canonical digits ≤32; Reddit: `[A-Za-z0-9_]` ≤64; X: `[A-Za-z0-9]` ≤64), mirrored by the
  design's Pattern/MaxLength and pinned against it by `internal/apivalidation/campaign_ref_id_drift_test.go`.
  Malformed ids are 400 before any lookup.
- **`list-reddit-ads-accounts`.** `GET /projects/{projectId}/connection-reddit-ads/accounts`
  (the sibling routes' shape, not `/reddit-ads/accounts`). `reddit.Client.ListAdAccounts` walks
  `GET /me/businesses` then `GET /businesses/{id}/ad_accounts`, paged via `walkPagesCapped`
  (the monitor's `walkPages` with the cap parameterised), page- and item-bounded, all or nothing:
  any failure — including a 5xx, timeout or a 429 that outlasts the bounded retry — is an error
  and a 503 at the API, never an empty list. `RedditDispatcher.ListAccounts` shares the new
  `validateRedditCredentials` with dispatch (minus the account-id check). No sibling marks the
  connection's bound account in the list, so this one does not either.
- **Chart:** reddit-ads moved into the shared discovery branch of the HTTPRoute regex (its
  separate `account-monitor`-only branch is gone); RuleSet entries for
  `connection-reddit-ads/accounts` and the three campaign-ref paths; parity rows both ways.
- **Prose that named Reddit as lacking discovery** was corrected: `ruleset.md`, `httproute.md`,
  `internal-dispatch`, `internal-bootstrap`, `sysacct.go`, the design comment on the monitor
  methods. Reddit now holds both discovery halves but is deliberately NOT added to
  `accountDiscoveryProviders`: its public config still Requires `account_id`.

**Verified only from documentation / not live:** the two Reddit operations come from Reddit's
published v3 reference; element field names and `pagination.next_url` follow this package's
existing unverified conventions; no request has been made against a live Reddit account. A
third-party report of `/me/businesses` answering 404 exists — if true, the route answers 503.
