# 2026-08-13 — Replace=true is one-time and transitional, not an invariant

**Fix** — `charts/lfx-v2-campaign-service/templates/deployment.yaml`,
`docs/knowledge/kubernetes/deployment.md` (linuxfoundation/lfx-self-serve#1541).
Corrects the scope claimed for the `argocd.argoproj.io/sync-options: Replace=true`
annotation added in #126 and marks it transitional. Follow-up from @bramwelt's PR #126
review.

## The scope was overstated

The original write-up said the flip "self-heals on every cluster and every re-create."
That is wider than the truth. The forbidden-field failure
(`spec.strategy.rollingUpdate: Forbidden ... when strategy type is 'Recreate'`) requires a
**pre-existing** Deployment that was first created as RollingUpdate — the API server
defaulted a `strategy.rollingUpdate` block on it that no field manager owns, so
server-side apply cannot strip it and it cannot coexist with `type: Recreate`. A **freshly
created** object emits `type: Recreate` directly and never gets that block, so it never
needed `Replace=true` at all. The real scope is **one-time per pre-existing environment**,
not a standing property of the chart.

The concept (`deployment.md`) now states this; the earlier log fragment is left as the
historical record.

## The cost outlives the reason

`Replace=true` makes ArgoCD PUT (replace) this Deployment on every sync instead of applying
it, which costs two things an ordinary apply does not: a live field the rendered manifest
omits is removed, and server-side-apply field-ownership conflict detection is bypassed, so a
value another controller owns is overwritten rather than flagged. `spec.replicas` is the
concrete case here — the chart pins it AND the ArgoCD Application sets
`ignoreDifferences: /spec/replicas` with `RespectIgnoreDifferences=true`, so an ordinary sync
leaves an out-of-band `kubectl scale` or an external HPA's value alone, but a full replace
overwrites it back to `replicaCount`. Low risk today (no HPA in the chart, `replicaCount: 1`),
but nothing reminds anyone to remove it. Both the
annotation comment and the concept now name it a transitional workaround and point at the
work that retires it: migrations move out of pod boot into a PreSync Job
(linuxfoundation/lfx-self-serve#1543), after which the Deployment returns to RollingUpdate
and the annotation is dropped (linuxfoundation/lfx-self-serve#1544). Once no pod migrates
the shared schema at boot, the cutover hazard the pairing exists to prevent is gone.

## Not a behavior change

Docs and one chart comment only — the applied Kubernetes object is unchanged (the raw
`helm template` text gains the new comment lines, but its non-comment content is identical),
so there is nothing to deploy for this entry.
