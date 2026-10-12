# 2026-10-07 — LFXV2-2665 X stats jobs: account-wide budget and pacer

**Fix** — Three #291 review threads on the X audience read's stats-job limits:

- **Ambiguous creates are charged.** A job create that failed ambiguously — a 5xx, a transport
  error, a throttled POST, or a 2xx whose body named no usable job — may have committed on X with
  an id we never learn. It was not counted, so refreshes could pile up uncounted jobs. The client
  now reports it (`AudienceJobsAbandonedError.Unknown`, also when it was the first create), and
  the guard records it as an ANONYMOUS outstanding job: it counts against the 12-job account
  budget, is never sent to X for reconciliation, and only expires with the 60-minute hold. A
  definite 4xx rejection charges nothing.
- **One pacer per ad account.** The stats-job slot reservation was atomic per client, but one
  process holds several clients for one account (two projects' connections to the shared LF
  account; cache replacement). New `twitter.AccountPacers` (`pacer.go`): every client the
  dispatcher builds shares one write pacer per ad account, so `pace` and `reserveStatsJobSlots`
  cannot interleave across them. Clients built without a registry still pace privately.
- **One replica while the flag is on.** Investigated first: the chart defaults to
  `replicaCount: 1` with no HPA, the deployment is `Recreate` "because replicaCount is 1", and the
  existing write pacer and other locks are documented as process-local. No env overlay in this repo
  runs more than one replica, so rather than a distributed design, `templates/deployment.yaml`
  now fails to render `TWITTER_METRICS_ENABLED` (either env input, or `valueFrom`) with more than
  one replica or autoscaling past one, and the API catalog, README, values and concepts say the
  limits are per process.
- Tests for each; each fails with its fix reverted.
