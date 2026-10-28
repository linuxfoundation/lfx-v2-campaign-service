# 2026-10-26 — Current compose partial shapes documented exactly (LFXV2-2770)

**Docs** — `docs/api-catalog.md` and [internal/service](../code/internal-service.md) said a current
compose partial carries only `master_name`, or that no current path sets `suppression`. The
recording shape (`ComposeAudienceMaster`'s record failure) carries the confirmed `master` AND, when
the full standard set was selected, `suppression` = the project's standing standard list. Both
docs now list the two current shapes (`master_name` alone; `master` with optional `suppression`),
tell a client to branch on `master` first, and mark the shapes without `master` that carry a
suppression as legacy. The `composePartialMessage` godoc says the same. No behaviour change.
