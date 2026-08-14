# Kubernetes

* [Deployment](deployment.md) - Helm Deployment for the campaign service, including PG*, CREDENTIAL_ENCRYPTION_KEY, SNOWFLAKE_* and the AI_PROXY_URL / AI_API_KEY credentials from lfx-v2-campaign-service-secrets, plus plain non-secret values such as AI_MODEL.
* [Migrate Job](migrate-job.md) - ArgoCD PreSync hook Job that runs `campaign-service migrate` before each rollout to apply schema migrations; the server no longer migrates at boot and only verifies the schema.
* [Middleware](heimdall-middleware.md) - Kubernetes Middleware manifest for the campaign service, defined in the Helm chart.
* [HTTPRoute](httproute.md) - Kubernetes HTTPRoute manifest for the campaign service, defined in the Helm chart.
* [PodDisruptionBudget](pdb.md) - Kubernetes PodDisruptionBudget manifest for the campaign service, defined in the Helm chart.
* [RuleSet](ruleset.md) - Kubernetes RuleSet manifest for the campaign service, defined in the Helm chart.
* [Service](service.md) - Kubernetes Service manifest for the campaign service, defined in the Helm chart.
* [ServiceAccount](serviceaccount.md) - Kubernetes ServiceAccount manifest for the campaign service, defined in the Helm chart.
