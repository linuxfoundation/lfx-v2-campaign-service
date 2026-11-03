# 2026-11-02 — multi-campaign cs-1: campaign funnel stage/tactic and a slot compare-and-set

**Update** — this is the first campaign-service step of letting one event run several campaigns on
the same platform, labelled by funnel stage and tactic.

**Migration.** `000054` adds `campaigns.funnel_stage` (`top`/`mid`/`lower`) and
`campaigns.tactic` (a closed list). Both are nullable; NULL means legacy. Only the dispatch claim's
INSERT writes them, so they are immutable afterwards, and they are not in the slot key.

**Create input.** `funnel_stage` and `tactic` are design Enums, re-checked in the service, and
accepted for microsoft-ads only (400 otherwise) until another platform puts the stage in its name.
`expected_slot_versions` is now REQUIRED with `new_version`, for every selected platform (400
otherwise). The orchestrator's new_version arm claims the next version only while the latest live
campaign is the one the caller saw. Otherwise it returns that campaign with `reused: true`, so a
double click, reload, second tab or late replay creates exactly one campaign. One residual: a
replay that arrives after the campaign it created was deleted creates another.

**Response.** Job platform results gain `campaign_row_id`, `slot_version`, `variant`,
`funnel_stage`, `tactic` and `reused`. The campaign resource and its index document gain
`variant`, `funnel_stage`, `tactic` and `created_at`.

**Names.** Dispatchers read the CLAIMED row's labels (`model.WithDispatchLabels`). A labelled
Microsoft campaign carries a `ToFU`/`MoFU`/`BoFU` segment in its name. An unlabelled one keeps its
exact legacy name.

**Unchanged.** The slot-version allowlist is still Microsoft only. The api-catalog delete row
now names the slot version it frees. `deployment.md` has the rollback runbook: turn off the
self-serve multi-campaign flag and settle in-flight labelled Microsoft rows before rolling the
image back.
