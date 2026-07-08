# Feature Specification: Campaign Service Deployment Configuration

**Feature Branch**: `feat-LFXV2-2558-deployment`

**Created**: 2026-07-08

**Status**: Draft

**Input**: User description: "We want to review and update the campaign service deployment configuration so that the service is deployable to the development, staging, and production kubernetes environments. In this workspace, I have added the lfx-v2-campaign-service git worktree feature branch (Jira ticket LFXV2-2558) for the chart and templating work. Also, I have added the lfx-v2-argocd git worktree feature branch folder which will need some modifications to add the new service to the list of apps/services to deploy. Finally, I have added lfx-v2-project-service and lfx-v2-committee-service main branches as references for reviewing the deployment configurations to ensure we follow the standard - these are examples and should be aligned and help identify all the relevant deployment configuration files."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Deploy campaign-service to the development environment (Priority: P1)

A platform engineer needs the campaign-service to run in the development Kubernetes
cluster. The service's Helm chart is reviewed against the platform standard (as
exemplified by project-service and committee-service) and corrected where it deviates,
and the GitOps repository is wired so ArgoCD automatically deploys the service to dev,
where it becomes healthy and reachable through the platform API gateway.

**Why this priority**: Development is the first and most-used target environment and the
prerequisite for any promotion to staging or production. A correct, standards-aligned
chart plus working dev registration is the minimum viable outcome: it proves the service
can be built, packaged, deployed, and routed end to end.

**Independent Test**: Render and lint the chart (`helm template` / `helm lint`), then let
ArgoCD sync the dev application; verify the ArgoCD app reports Synced/Healthy, the pod
passes its liveness/readiness probes, and the service responds through the dev gateway
hostname.

**Acceptance Scenarios**:

1. **Given** the campaign-service Helm chart on the feature branch, **When** the chart is
   rendered and linted, **Then** it produces valid Kubernetes manifests with no lint
   errors and matches the standard template set used by the reference services.
2. **Given** the chart's image, service, and probe configuration, **When** the image
   repository path is compared across `values.yaml`, the local example values, and the
   Makefile, **Then** they reference a single consistent registry path.
3. **Given** the dev entry in the ArgoCD `lfx-v2-applications` ApplicationSet and the
   dev/global values files, **When** ArgoCD syncs, **Then** the campaign-service
   application reports Synced and Healthy in the dev cluster.
4. **Given** the deployed service, **When** its liveness and readiness probes are
   evaluated, **Then** the pod becomes Ready and the service is reachable at the dev
   gateway hostname on its configured route.

---

### User Story 2 - Promote campaign-service to the staging environment (Priority: P2)

A release manager promotes a published campaign-service release to staging. The staging
ApplicationSet entry references a pinned chart version from the OCI registry, and the
staging values file is activated with a pinned image tag and staging-specific
configuration (domain, service account role, environment variables).

**Why this priority**: Staging validates a pinned, immutable release before it reaches
production. It depends on P1 (a correct chart and a published release artifact) but is
independently valuable as a pre-production verification gate.

**Independent Test**: With a published OCI chart version, add the staging list entry and
activate the staging values file, then verify ArgoCD syncs the staging application to
Synced/Healthy using the pinned chart and image tag, reachable at the staging hostname.

**Acceptance Scenarios**:

1. **Given** a published OCI chart version for campaign-service, **When** the staging
   ApplicationSet entry is added, **Then** it uses the OCI chart source with a pinned
   chart version and the correct namespace, matching the reference-service pattern.
2. **Given** the staging values file, **When** it is activated, **Then** it pins an
   immutable image tag and sets staging domain, service account annotations, and
   environment configuration consistent with the reference services.
3. **Given** the staging registration, **When** ArgoCD syncs, **Then** the staging
   application reports Synced and Healthy and is reachable at the staging gateway
   hostname.

---

### User Story 3 - Promote campaign-service to the production environment (Priority: P3)

A release manager promotes the validated staging release to production. The production
ApplicationSet entry references the pinned OCI chart version, and the production values
file is activated with a pinned image tag and production-specific configuration.

**Why this priority**: Production is the final destination and highest-risk environment.
It follows the same GitOps promotion pattern as staging and is gated on successful
staging validation, so it carries the lowest implementation priority even though it is
the ultimate goal.

**Independent Test**: With a validated release, add the production list entry and activate
the production values file, then verify ArgoCD syncs the production application to
Synced/Healthy using pinned chart and image versions, reachable at the production
hostname.

**Acceptance Scenarios**:

1. **Given** a validated release, **When** the production ApplicationSet entry is added,
   **Then** it uses the OCI chart source with a pinned chart version and the correct
   namespace.
2. **Given** the production values file, **When** it is activated, **Then** it pins an
   immutable image tag and sets production domain, service account annotations, and
   environment configuration.
3. **Given** the production registration, **When** ArgoCD syncs, **Then** the production
   application reports Synced and Healthy and is reachable at the production gateway
   hostname.

---

### Edge Cases

- **Missing upstream secrets**: If the AWS Secrets Manager secrets tagged for
  campaign-service do not yet exist, the ExternalSecret cannot sync. The configuration
  must be correct, but successful secret population is an external DevOps prerequisite
  that must be called out as a dependency rather than silently failing.
- **Chart/image not yet published**: Staging and production reference pinned OCI chart
  versions and image tags that only exist after the first release is cut. Their
  registration must be complete and correct but activation is gated on a published
  release.
- **Namespace inconsistency**: The reference services use varying namespace names
  (e.g. `committee-service` vs. `project-service`). Campaign-service must use one
  consistent namespace across all environments and all configuration files.
- **Empty or incomplete authorization rules**: The gateway authorization ruleset must be
  internally consistent and deployable; where the current service surface does not yet
  require per-endpoint authorization rules, that gap must be intentional and documented
  rather than accidental.
- **Umbrella-chart double-deployment**: If campaign-service is ever added to the platform
  umbrella chart, it must be disabled there to avoid being deployed twice (once by the
  umbrella and once as its own ArgoCD application).
- **Configuration drift between repos**: NATS resource names, bucket names, subjects, and
  environment variable names declared in the chart must match what the service code
  expects; drift between the chart and the code must be caught during review.

## Requirements *(mandatory)*

### Functional Requirements

#### Chart conformance (campaign-service repository)

- **FR-001**: The campaign-service Helm chart MUST be reviewed against the platform
  standard exemplified by project-service and committee-service, and every unintended
  deviation MUST be corrected.
- **FR-002**: The chart MUST render and lint cleanly, producing valid Kubernetes manifests
  for all supported value combinations.
- **FR-003**: The container image repository path MUST be consistent across all locations
  that reference it (default chart values, local example values, and build tooling).
- **FR-004**: The chart MUST include the standard resource templates required for this
  service to deploy and be routed (workload, service, service account, gateway route,
  gateway authorization, pod disruption budget), matching the reference-service template
  set, and MUST omit resource templates the service does not require.
- **FR-005**: The workload MUST expose liveness and readiness health checks on the
  service's health endpoints, following the reference-service probe pattern.
- **FR-006**: The gateway route MUST expose the campaign-service's public path(s) under the
  platform API hostname, following the reference-service routing pattern.
- **FR-007**: The gateway authorization ruleset MUST be internally consistent and
  deployable; any endpoints not yet requiring authorization rules MUST be handled
  intentionally and documented.
- **FR-008**: The chart MUST support service-account configuration compatible with the
  secret-injection mechanism used by comparable services (i.e. workload identity for
  external secret access).
- **FR-009**: The chart MUST parameterize image tag and per-environment overrides so that
  the GitOps repository can supply environment-specific values without modifying the
  chart.
- **FR-010**: The repository MUST provide automation to publish the chart as a versioned
  OCI artifact and the image to the container registry, following the reference-service
  release workflow, so that staging and production can reference pinned versions.
- **FR-011**: Any NATS resources, environment variable names, and configuration keys
  declared in the chart MUST match what the service code actually consumes; unused
  forward-looking configuration MUST be identified during review.

#### GitOps registration (lfx-v2-argocd repository)

- **FR-012**: Campaign-service MUST be registered in the development ApplicationSet using
  the development chart source pattern (tracking the service repository) with the correct
  namespace and custom-resources flag.
- **FR-013**: Campaign-service MUST be registered in the staging ApplicationSet using the
  OCI chart source pattern with a pinned chart version and the correct namespace.
- **FR-014**: Campaign-service MUST be registered in the production ApplicationSet using
  the OCI chart source pattern with a pinned chart version and the correct namespace.
- **FR-015**: The GitOps repository MUST provide the layered values files
  (global, dev, staging, prod) for campaign-service, following the reference-service
  layering, with the global file holding shared baseline settings and each environment
  file holding its overrides.
- **FR-016**: Staging and production values files MUST pin immutable image tags; the
  development values file MUST use the mutable development image tag and pull policy.
- **FR-017**: Each environment values file MUST set the correct gateway domain,
  namespace, service account annotations (workload identity role), and environment
  variables for that environment.
- **FR-018**: The external-secret configuration for campaign-service MUST be present and
  correct in the GitOps repository, referencing the service's synced secret, and the
  global values MUST wire those secret references into the workload.
- **FR-019**: A single consistent namespace MUST be used for campaign-service across all
  ApplicationSet entries, values files, and custom resources.
- **FR-020**: Campaign-service MUST be disabled in the platform umbrella chart
  configuration if and when it is added there, to prevent double deployment.
- **FR-021**: The GitOps repository's service inventory documentation (e.g. README
  service table) SHOULD be updated to list campaign-service, and a service-catalog entity
  SHOULD be added following the existing pattern.

#### Cross-cutting

- **FR-022**: All new and modified source/config files MUST include the required license
  header where applicable.
- **FR-023**: The review MUST produce an explicit, verifiable inventory of every
  deployment configuration file for campaign-service (in both repositories) and its
  status relative to the standard, so no required file is missed.
- **FR-024**: External prerequisites that are outside the two repositories (workload
  identity IAM roles per environment, AWS Secrets Manager secrets, first published
  release artifacts) MUST be documented as dependencies with the environment they block.

### Key Entities

- **Helm chart (campaign-service)**: The packaged deployment definition living in the
  service repository; contains chart metadata, default values, and resource templates;
  published as a versioned OCI artifact for staging/production.
- **ApplicationSet entry**: One list element per environment in the GitOps
  `lfx-v2-applications` ApplicationSet that tells ArgoCD which chart source, version,
  namespace, and custom-resources flag to use for campaign-service.
- **Values files (layered)**: The GitOps override files (global + per-environment) that
  supply environment-specific configuration on top of the chart defaults.
- **Custom resources (external secrets)**: The SecretStore and ExternalSecret manifests
  that let campaign-service consume secrets from the external secret provider via
  workload identity.
- **Environment**: A deployment target (development, staging, production) with its own
  cluster, gateway domain, identity account, image-tag strategy, and chart-source
  strategy.
- **Reference services**: project-service and committee-service, whose deployment
  configuration defines the standard campaign-service must conform to.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The campaign-service chart renders and lints with zero errors, and its
  template set matches the platform standard (no missing standard resource, no
  unnecessary resource).
- **SC-002**: Campaign-service deploys to the development environment via ArgoCD and
  reports Synced and Healthy, with its pod passing liveness and readiness checks.
- **SC-003**: Campaign-service is reachable through the platform API gateway at the
  correct hostname in each environment where it is activated.
- **SC-004**: Staging and production registrations reference pinned, immutable chart
  versions and image tags (no mutable/floating references in staging or production).
- **SC-005**: A single, consistent namespace and service name is used for
  campaign-service across every configuration file in both repositories.
- **SC-006**: A complete file-by-file inventory exists showing that every deployment
  configuration file required for all three environments is present and conformant, with
  any remaining items attributable only to documented external prerequisites.
- **SC-007**: 100% of the deviations identified against the reference-service standard are
  either corrected or explicitly documented as intentional with rationale.
- **SC-008**: All external dependencies that gate deployment to each environment are
  documented, so a reader can determine exactly what non-repository prerequisites remain.

## Assumptions

- The default spec directory `specs/` is used with sequential numbering; this feature is
  `002-deployment-config`.
- The platform standard for deployment configuration is defined by project-service and
  committee-service on their main branches; where the two references differ, the newer or
  more secure convention (e.g. committee-service's map-based environment variables and
  service-account/IRSA usage) is preferred because campaign-service uses external secrets.
- Campaign-service consumes secrets from the external secret provider, so it follows the
  committee-service pattern (ServiceStore + ExternalSecret + service-account workload
  identity) rather than the simpler project-service pattern.
- The canonical namespace for campaign-service is `lfx-v2-campaign-service`, matching the
  campaign chart's existing convention, applied consistently across both repositories.
- The current campaign-service application surface is limited (health endpoints and
  campaign routes); NATS buckets/streams and full per-endpoint authorization rules are
  only included to the extent the current service code requires them, and broader
  application data/authorization wiring is out of scope for this deployment-configuration
  effort.
- Development tracks the service repository at HEAD with the mutable development image
  tag; staging and production reference pinned OCI chart versions and image tags.
- Staging and production activation depends on the first published OCI chart version and
  image tag; their configuration is completed and made correct now, and activation
  proceeds once the release artifacts exist.
- Secret values/rotation, per-environment workload-identity IAM roles, and creation of the
  tagged secrets in the external provider are DevOps/CloudOps handoffs external to these
  two repositories; this feature owns only the references and configuration, not the
  secret material or IAM provisioning.
- Backstage catalog entries and README service-inventory updates are recommended
  documentation steps and are not blocking for deployment.
