# 2026-09-25 Add an ingress NetworkPolicy

**Update** — Added an optional Helm NetworkPolicy for the campaign API. Local chart defaults leave it
disabled; Argo CD deployed values allow ingress only from Traefik pods in the shared `lfx` namespace.
