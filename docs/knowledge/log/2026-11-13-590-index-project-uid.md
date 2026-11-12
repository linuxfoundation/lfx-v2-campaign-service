# 2026-11-13 — #590 Index briefs and campaigns under the project UID

**Fix** — The Query Service returned no briefs or campaigns to any reader. Each indexed document's
FGA check was `campaign_manager` on `project:<slug>`, but OpenFGA keys projects by UID, so no
tuple matched (lfx-self-serve#5380).

- `indexer.Relay.WithProjectUIDResolver` makes the relay resolve the slug through
  `lfx.projects-api.slug_to_uid` (`accesscheck.Checker.ProjectUID`, now exported) when it
  publishes. It then sends `access_check_object` and `history_check_object` as `project:<uid>`,
  and `parent_refs` as `project:<uid>` and `project:<slug>`.
  - A failed lookup, or a non-UUID answer, leaves the row pending. The failure is cached
    for 30 seconds, so other rows of that project fail fast.
  - A delete is published without a lookup.
  - A UID-keyed message and the stored row are unchanged.
  - Resolved UIDs are cached for 30 seconds, about one pass.
  - A definite `not_found` is published as written (slug-keyed, readable by nobody), so the
    resource's later delete is not held behind it.
  - Rollout order: flag on, then token. The container warns when it has a token and the flag is off.
- Behind `INDEXER_RESOLVE_PROJECT_UID`, which is on only when exactly `"true"`. The chart
  defaults it to `"false"`.
- Rows already pending (all of them in an environment whose relay is idle for want of
  `INDEXER_SERVICE_TOKEN`) publish UID-keyed once the flag is on. Documents already indexed
  slug-keyed are corrected by their next write.
- Tests:
  - `TestRelay_KeysFGAChecksOnTheProjectUID`
  - `TestRelay_UnresolvedProjectLeavesTheRowPending`
  - `TestRelay_ResolvesEachSlugOnceAndSkipsUIDs`
  - `TestRelay_WithoutAResolverKeepsTheSlug`
  - `TestProjectUID`
  - `TestRelay_DeletesPublishWithoutALookup`
  - `TestRelay_RemembersAFailedLookup`
  - `TestRelay_RefusesANonUIDAnswer`
  - `TestIndexRelayResolver`, `TestNewIndexRelay_NoResolverWithoutAConnectionOrTheFlag` and
    `TestIndexRelayIsOnlyBuiltByNewIndexRelay`, which pin the container wiring
  - `TestLoadIndexerResolveProjectUIDFromEnv`, which pins that only `"true"` turns the flag on
