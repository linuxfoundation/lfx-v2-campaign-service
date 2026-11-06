{{- /*
Copyright The Linux Foundation and each contributor to LFX.
SPDX-License-Identifier: MIT
*/}}
{{- /*
  lfx-v2-campaign-service.xStatsMultiReplica renders "true" when the X (Twitter) stats-job
  features are EFFECTIVELY on together with more than one replica, and nothing otherwise. It is
  an ADVISORY signal, rendered as a warning in templates/NOTES.txt — never a render failure (#381).

  Why advisory. TWITTER_METRICS_ENABLED turns on the X account monitor and the X audience read,
  which create asynchronous stats jobs on X ad accounts shared across foundations (X allows 100
  concurrent jobs per account). Their limits — the audience read's per-account job budget,
  singleflight and slot (internal/dispatch/twitter_audience_guard.go) and the account-keyed write
  pacer (internal/platform/twitter/pacer.go) — are process-local, so the binding guarantee is made
  at RUNTIME: only the pod holding the per-account Postgres advisory lease
  (internal/infrastructure/postgres/stats_job_lease.go) creates stats jobs — the audience read
  (internal/dispatch/twitter_audience.go) AND the account monitor's report submission
  (TwitterDispatcher.SubmitAccountReport, internal/dispatch/twitter_monitor.go) both call
  ownStatsJobs before contacting X — and every other pod refuses. And nothing polls either path:
  no ticker, cron or subscriber in internal/ or cmd/ reaches ConnectionService.MonitorTwitterAdsAccount
  or the audience read, and the lfx-self-serve Monitoring tab reads the X panel only on a user
  action (platform, date range, the panel's Refresh button) with no interval refresh. So more
  replicas than one mean a non-owner pod answers some X reads 503 or serves the saved report,
  not that the account's job limit is multiplied.

  An earlier revision `fail`ed the render here instead. Deployments set three replicas
  (lfx-v2-argocd values/global/lfx-v2-campaign-service.yaml), so turning the flag on in a deployed
  environment would have made `helm template` fail and ArgoCD unable to generate the manifest AT
  ALL, blocking every unrelated deploy of this service — the incident the retired Reddit guard
  caused (#378). Do not reinstate a render-time `fail` here.

  It judges the EFFECTIVE value the container gets. The env list renders app.environment first
  and app.extraEnv after it, and Kubernetes resolves a repeated name to its LAST occurrence, so
  the last extraEnv entry named TWITTER_METRICS_ENABLED wins over app.environment. An
  app.environment entry renders `value` when the key is present and `valueFrom` otherwise. A
  valueFrom on the effective entry cannot be inspected and is treated as on. It cannot see an
  out-of-band scale or an external HPA (ArgoCD ignores /spec/replicas); the lease covers those.
*/}}
{{- define "lfx-v2-campaign-service.xStatsMultiReplica" -}}
{{- $xStats := false }}
{{- with (get .Values.app.environment "TWITTER_METRICS_ENABLED") }}
  {{- if hasKey . "value" }}
    {{- $xStats = eq (toString .value) "true" }}
  {{- else if .valueFrom }}
    {{- $xStats = true }}
  {{- end }}
{{- end }}
{{- range $entry := .Values.app.extraEnv }}
  {{- if eq (toString (default "" $entry.name)) "TWITTER_METRICS_ENABLED" }}
    {{- $xStats = or (and $entry.valueFrom true) (eq (toString $entry.value) "true") }}
  {{- end }}
{{- end }}
{{- if $xStats }}
  {{- $replicas := int (default 1 .Values.replicaCount) }}
  {{- $autoscaleMax := 1 }}
  {{- with .Values.autoscaling }}{{- if .enabled }}{{- $autoscaleMax = int (default 2 .maxReplicas) }}{{- end }}{{- end }}
  {{- if or (gt $replicas 1) (gt $autoscaleMax 1) }}true{{- end }}
{{- end }}
{{- end }}
