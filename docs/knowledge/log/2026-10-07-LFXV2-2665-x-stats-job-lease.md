# 2026-10-07 — LFXV2-2665 X stats jobs: one pod owns them

**Fix** — Five #296 review threads:

- **Runtime single-owner lease.** The chart's replica guard cannot see an out-of-band scale or an
  external HPA (ArgoCD ignores `/spec/replicas`), so several pods could each run their own
  process-local X job budget and pacer. New `domain.StatsJobLease`, implemented by
  `postgres.StatsJobLease`: a session advisory lock per X ad account (two-int key space, no
  migration) on a dedicated connection. The X dispatcher asks it before the audience read
  contacts X and before the account monitor submits a report; a pod without the lease answers
  the audience read 503 ("another instance owns X stats jobs; retry") and the monitor's
  submission fails in its existing transient class (logged, saved report served). A held lease
  is re-verified (connection ping) on every call, so losing the session stops new submissions
  at the next call. The container binds it on both wiring paths and releases it before closing
  the pool. The chart guard stays as a first line of defence.
- **Effective flag value in the chart guard.** The guard checked `app.environment` and
  `app.extraEnv` independently, so a later extraEnv `"false"` still read as on. It now computes
  the value the container gets: the env list renders `app.environment` then `app.extraEnv`, and
  Kubernetes keeps a repeated name's last occurrence; a `valueFrom` on the effective entry counts
  as on.
- **JSON specs.** Verified on origin/main: all eight published documents (v2/v3, JSON/YAML,
  gen/ and kodata) already carry the same operation text — the JSON files are single-line, which
  hides them in a diff review. `make apigen` changes nothing. New
  `TestPublishedSpecsAgreeOnOperationText` turns any future drift into a failure.
- **Pacer comments.** `pace`, `TwitterDispatcher.clients` and the credcache roster no longer
  describe the write pacer as per client instance; it is per ad account (`AccountPacers`).
- Tests for each; each fails with its fix reverted.
