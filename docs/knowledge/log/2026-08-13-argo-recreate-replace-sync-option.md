# 2026-08-13 — ArgoCD could not deploy the Recreate flip without a full replace

**Fix** — `charts/lfx-v2-campaign-service/templates/deployment.yaml`,
`charts/lfx-v2-campaign-service/parity_test.go`,
`docs/knowledge/kubernetes/deployment.md`. Adds the metadata annotation
`argocd.argoproj.io/sync-options: Replace=true` to the Deployment, merges it into any
operator-supplied `sync-options`, and pins both the default render and the merge path in
`parity_test.go` (`TestDeploymentUsesRecreateStrategy` and
`TestDeploymentMergesReplaceIntoOperatorSyncOptions`).

## The strategy was correct and still undeployable

`v1.0.4` shipped `strategy.type: Recreate` (commit `e37b136f`) so a backward-incompatible
boot migration cannot go live under the previous pod. The chart render was right and the
ArgoCD Application was already pointed at `1.0.4`, yet the sync failed with:

```text
Deployment.apps "lfx-v2-campaign-service" is invalid: spec.strategy.rollingUpdate:
Forbidden: may not be specified when strategy `type` is 'Recreate'
```

The failure is not in the rendered manifest — it never emits `rollingUpdate`. It is a
property of flipping an EXISTING object. The live Deployment was created as RollingUpdate,
so the API server DEFAULTED a `strategy.rollingUpdate` block (maxSurge/maxUnavailable
25%). That block is owned by no field manager, so the Application's `ServerSideApply=true`
sync will not strip it, and `rollingUpdate` may not coexist with `type: Recreate`. Every
sync therefore re-proposed Recreate against an object that still carried the defaulted
field, was rejected, and left the object untouched at RollingUpdate — a loop.

## Why the obvious manual patch did not stick

`kubectl patch --type=json -p '[{"op":"remove","path":"/spec/strategy/rollingUpdate"}]'`
removed only the sub-field and left `type: RollingUpdate`. The Deployment defaulter
re-populates `rollingUpdate` whenever `type` is RollingUpdate, so the field reappeared
immediately (the patch even reported `no change` once, because the object had momentarily
been Recreate between reconciles). The write that sticks sets both in one operation:

```bash
kubectl -n lfx-v2-campaign-service patch deployment lfx-v2-campaign-service \
  --type=merge -p '{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}'
```

With `type: Recreate` the defaulter no longer adds the surge block, and the `1.0.4` sync
matched cleanly.

## The durable fix is a full replace, not a manual step

The manual patch is a one-time cutover, not a property of the chart. `Replace=true` makes
ArgoCD apply this Deployment with `kubectl replace` instead of a merge/SSA patch, so any
orphaned defaulted field is discarded on apply and the flip self-heals on every cluster
and every re-create. Scoped to the Deployment via annotation rather than the shared
ApplicationSet `syncOptions`, so only this object gets replace semantics. `Replace=true` is
MERGED into any operator-supplied `sync-options` rather than emitted as a bare literal:
`argocd.argoproj.io/sync-options` is one comma-separated value, so a literal ahead of the
`.Values.annotations` block would be a duplicate map key an override wins last, silently
dropping `Replace=true`. The two settings are now a unit — `Recreate` without `Replace=true`
reopens the exact forbidden-cutover failure — so `TestDeploymentUsesRecreateStrategy`
asserts both on the default render and `TestDeploymentMergesReplaceIntoOperatorSyncOptions`
pins the merge/override path, revert-verified.
