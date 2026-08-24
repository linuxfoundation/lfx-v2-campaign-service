# 2026-08-18 — LFXV2-2641 Google Ads keyword/audience reads and keyword actions

**Creation** — the last three endpoints of LFXV2-2641. The ticket's metrics endpoints and the
status toggle already shipped; these are `GET /projects/{projectId}/google-ads/keywords`,
`GET /projects/{projectId}/google-ads/audience` and
`POST /projects/{projectId}/briefs/{briefId}/campaigns/{id}/keyword-actions`, taken verbatim
from `docs/api-catalog.md` rather than from a summary — the two reads are project-scoped and
sit under the Monitoring surface, while only the mutation is nested under a campaign.

**These reads do not breach the Query-Service rule, and the catalog already says why.** Rule 3
and architecture D5 forbid bespoke list and `*_audit` endpoints **for briefs and campaigns** —
the resources this service stores and indexes. The catalog adjudicates this exact question one
paragraph below the row that defines these endpoints, for the sibling `/accounts` route: a
live, credential-scoped read of what exists UPSTREAM at the provider "enumerates nothing this
service stores". Keywords and demographics are read through to Google Ads on every request and
are persisted in no table here, so the same reasoning applies unchanged. This was checked
deliberately because LFXV2-3099 built a `GET .../campaigns` list route and withdrew it (PR
#117) for breaching exactly this rule — but that route enumerated `campaigns`, a stored,
indexed resource, which is the distinction the rule turns on.

**`keyword-actions` is a batch, and rule 5 does not forbid it.** Rule 5 bans bulk mutation
because a single call cannot cleanly express partial success and cuts across per-target
PERMISSION boundaries. Every criterion in this batch belongs to the one campaign named in the
path — that campaign is the single permission-evaluated target — and partial success is not
representable because the upstream mutate is atomic. The batch is one campaign's keywords, not
many campaigns.

**A mutation on live spend gets the create path's guard ordering.** Everything refusable
locally is refused before Google is contacted: the batch is validated first (so a permanent
input fault never masquerades as a connection problem and no credential is decrypted for a
request that cannot succeed), then provisioning, then that every criterion belongs to THIS
campaign's ad group, then the account-identity check the status toggle already enforces. That
last one matters more here than on the toggle: criterion ids are bare numerics unique only
within their customer, a connection can be re-pointed between create and action, and `REMOVE`
is irreversible — so without it a mutate could permanently delete another account's keyword.
The ad-group check is what stops a caller pausing an arbitrary criterion from the shared
account through a campaign they happen to own.

**`applied_count` counts confirmed outcomes, not the request.** The two are equal on every
happy path, which is exactly why the distinction needed its own test: a handler reporting
`len(p.Actions)` passed every ordinary case while claiming success for keywords the platform
never confirmed. The fake now returns fewer outcomes than requested to separate them.

**Three mutations survived their first revert and each was a real gap.** The unprovisioned-
campaign guard was redundantly covered — a campaign with no platform id also has no ad group,
so deleting the first guard broke nothing until a case carried a valid ad group and nothing
else. `applied_count`'s source was unpinned, as above. Both tests were strengthened until the
revert failed; the guards were not weakened to match.

**Truncation is a signal, not a silent cap.** The keyword query asks for one row beyond the cap
so a full page is distinguishable from a truncated one. A cap without that flag is a wrong
answer rather than an incomplete one: a caller receiving exactly 50 rows would total them as
the account's spend.

**Audience aggregates because device segments campaigns.** `segments.device FROM campaign`
returns one row per (campaign, device), so buckets are summed by value and CTR is computed
after aggregation. Assuming one row per bucket would have reported the last campaign's numbers
as the whole account's — a wrong number that looks entirely plausible.
