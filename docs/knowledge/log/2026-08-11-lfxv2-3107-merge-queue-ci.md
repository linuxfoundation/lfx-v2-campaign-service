# 2026-08-11 — CI checks now also run on merge-group events

**Update** — `.github/workflows/{lfx-v2-campaign-service-build,mega-linter,license-header-check,validate-okf}.yml`
(LFXV2-3107). Added `merge_group` alongside `pull_request` so these checks
report against a merge queue's `gh-readonly-queue/main/...` ref once a queue
is enabled on `main`; without this, a queue would wait out the check-response
timeout and dequeue every entry.

`license-header-check.yml` also dropped an unused `pull-requests: write`
permission — the reusable workflow it calls only reads repo contents and
never mutates the PR, matching a fix already merged upstream
(`linuxfoundation/lfx-public-workflows#12`).

`validate-okf.yml` keeps its `pull_request.paths` filter but declares
`merge_group` unconditionally rather than trying to mirror the filter: the
`merge_group` event has no `paths`/`paths-ignore` support, so a `paths` key
under it is silently ineffective.

Enabling the merge queue ruleset itself is out of scope for this change and
is done manually in GitHub once these checks are confirmed to report on
`merge_group`.
