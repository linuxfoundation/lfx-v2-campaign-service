# 2026-10-13 — Tenancy tables, unserved metrics routes and discovery admission aligned with code (LFXV2-2665)

**Fix** — four doc/contract drifts, each checked against `origin/main` first:

- **Account tenancy (#243).** `docs/architecture.md` ("Account Tenancy" and "Current Platform
  Accounts") and `docs/channel-connections-schema.md` ("Current Account Inventory") listed Meta,
  Reddit and X as per-foundation accounts. Those platforms share one account across foundations;
  only LinkedIn runs several (TLF, LF Events). All three tables now say so, and `architecture.md`
  states that it — not the migrations, which do not encode tenancy — is the authority for it. The
  "this table is stale" notes in `internal/dispatch/linkedin.go`, `internal/dispatch/meta.go` and
  [Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md) now point at the doc
  instead. The two `2026-10-02-LFXV2-2023-account-scope-*` log entries are dated history and stay
  as written.
- **Unserved `/{provider}/metrics` routes.** The HTTPRoute regex and the `project-api` RuleSet
  routed and authorized `/projects/:projectId/{google-ads,linkedin-ads,meta-ads,reddit-ads,twitter-ads}/metrics`,
  which no `design/` method declares. Only that alternative was removed from the regex, together
  with the five RuleSet entries, and `parity_test.go` now pins all five as rejected by both
  matchers. The `docs/api-catalog.md` Monitoring section drops the design-sketch row and its
  warnings. See [HTTPRoute](../kubernetes/httproute.md) and [RuleSet](../kubernetes/ruleset.md).
- **LinkedIn monitor scope check.** The account-monitor concept said LinkedIn had not had the
  Google-class scope-filter check. `fetchAccountCampaignList` already filters to ACTIVE/PAUSED, so
  the concept now says LinkedIn lacks that defect, and that a full Reddit-style audit is still
  outstanding.
- **Microsoft in `accountDiscoveryProviders` (#181): not admitted.** Admission is both gates at
  once, as X's was: the bootstrap map AND dropping `Required("account_id")` from the public
  Microsoft connection config (a `design/` change and Goa regeneration). That is a public-API
  behaviour change, and #181 asks for a decision first. The reason is recorded in
  [internal/bootstrap](../code/internal-bootstrap.md).
