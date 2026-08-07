# 2026-08-06 — GA-4: doc accuracy and the ACTIVATE success-path test
**Update** — Corrected two stale comments in the GA-4 branch and added the missing happy-path
test for the ACTIVATE cascade.

1. **`ToggleStatus` doc comment (`internal/dispatch/googleads.go`)** — the comment still said
   "Today, only PAUSE is implemented" and then went on to describe the enabled ACTIVATE path
   immediately below it. GA-4 is what enables ACTIVATE, so the sentence was self-contradictory
   in the very change that made it false. Rewritten to state that both directions cascade, in
   opposite orders: PAUSE campaign-first, ACTIVATE children-first.

2. **`Container.Close` shutdown-budget comment (`internal/container/container.go`)** — the
   comment attributed three separately-budgeted phases to `Orchestrator.Shutdown`, including the
   `sweeperStopTimeout` sweeper stop. Verified against the implementation: `Close` performs the
   `sweeperStopTimeout` wait ITSELF (`select { case <-c.sweepDone: case <-time.After(...) }`)
   before it calls `c.orch.Shutdown(ctx, dispatchDrainTimeout)`. `Orchestrator.Shutdown` owns only
   two budgeted phases — the clean dispatch drain and the post-cancel grace — plus an unbudgeted
   cancel of its own periodic recovery sweeper. The ctx budget requirement (the full
   `ContainerCloseTimeout`) is unchanged and still correct; only the attribution was wrong.

3. **`TestGoogleAds_ToggleStatus_ActivateSucceedsChildrenFirst`** — every existing ACTIVATE test
   covered a refusal or a failure path, so nothing pinned the case GA-4 exists to enable. The new
   test provisions keyword criteria, asserts `ToggleStatus` returns nil, and asserts all three
   mutates are issued in child-first order (adGroups, adGroupAds, campaigns). The ordering is the
   safety property: the campaign gate must open last so no traffic is served against a still-paused
   ad group or ad. Verified binding — moving the campaign mutate ahead of the children fails the
   test on both the call count and the order.
