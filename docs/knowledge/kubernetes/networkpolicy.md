---
type: "Kubernetes Resource"
title: "NetworkPolicy"
description: "Optional ingress NetworkPolicy for the campaign service, configurable for deployed ingress peers."
resource: "charts/lfx-v2-campaign-service/templates/networkpolicy.yaml"
---

# NetworkPolicy

The policy is disabled by default for local development. When enabled, it selects campaign-service
pods and permits TCP ingress on the service port only from the configured peers. Deployed ingress
peers are configured by the Argo CD environment values.

See [charts/lfx-v2-campaign-service/templates/networkpolicy.yaml](../../../charts/lfx-v2-campaign-service/templates/networkpolicy.yaml).
