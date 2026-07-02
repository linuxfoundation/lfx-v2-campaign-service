# Quickstart & Validation: Health Endpoints (Readyz & Livez)

This guide validates the feature end-to-end: local build/format/lint/test, running the service
and probing the endpoints, then packaging and deploying into the local Kubernetes cluster and
confirming the probes pass. See [contracts/README.md](./contracts/README.md) for the endpoint
contract and [data-model.md](./data-model.md) for the readiness predicate.

## Prerequisites

- Go 1.24.2 (matches `go.mod`)
- `make`
- For container/deploy validation: Docker or `ko`, a local Kubernetes cluster (e.g., OrbStack/`k8s.orb.local`), `kubectl`, `helm`
- One-time tooling: `make deps` (installs `goa`), `make setup-dev` (installs `golangci-lint`)

## 1. Generate, format, lint, build, test (local)

```bash
make apigen     # generate gen/ from design/ (required first — scaffold won't compile without it)
make fmt        # NEW: go fmt ./... + gofmt -s -w
make lint       # golangci-lint
make test       # go test -race -coverprofile
make build      # local binary at bin/lfx-v2-campaign-service
```

**Expected**: `apigen` creates `gen/`; `fmt` leaves no diff; `lint` prints `Lint OK`; `test` passes
including the new `Livez`/`Readyz` unit tests; `build` produces the binary.

Production build (static/release binary, distinct from the container image):

```bash
make build-release   # NEW target: CGO-disabled linux binary with version ldflags
```

## 2. Run and probe locally

```bash
./bin/lfx-v2-campaign-service -p 8080 &
```

Validate the contract:

```bash
curl -i http://localhost:8080/livez     # expect 200, Content-Type text/plain, body "OK\n"
curl -i http://localhost:8080/readyz    # expect 200, body "OK\n" once initialized
```

**Expected outcomes** (map to acceptance scenarios):

- `/livez` → `200 OK`, `text/plain`, body `OK\n` (US1)
- `/readyz` → `200 OK`, `text/plain`, body `OK\n` when ready (US2)
- Neither request requires an `Authorization` header (FR-004)
- A `/readyz` call before the service is ready returns `503` (US2 scenario 2) — exercised via the unit test's not-ready case

Stop the local server when done (`kill %1`).

## 3. Package the container image

```bash
make docker-build            # Docker path
# or, multi-arch via ko (mirrors CI):
KO_DOCKER_REPO=ko.local ko build ./cmd/campaign-service --platform linux/amd64,linux/arm64 --sbom spdx
```

**Expected**: an image is produced (multi-arch + SBOM via the `ko` path).

## 4. Deploy to the cluster and verify probes

```bash
make helm-install-local      # or: helm upgrade --install ... (see Makefile)
kubectl -n lfx rollout status deploy/lfx-v2-campaign-service
kubectl -n lfx get pods -l app=lfx-v2-campaign-service
kubectl -n lfx describe deploy/lfx-v2-campaign-service | grep -A2 -i 'Liveness\|Readiness\|Startup'
```

**Expected outcomes** (US3 / SC-004 / SC-005):

- The pod reaches `Ready 1/1` without manual intervention.
- `livenessProbe (/livez)`, `readinessProbe (/readyz)`, and `startupProbe (/readyz)` all succeed.
- The instance only enters serving rotation after `/readyz` reports ready.

## 5. CI packaging validation

- Open a PR: `lfx-v2-campaign-service-build.yaml` runs `deps → apigen → build → test` (green).
- Branch/PR image publish (`ko-build-branch.yaml`) runs (no longer `if: false`) and produces a tagged image.
- On a release tag: `ko-build-tag.yaml` builds multi-arch, produces an SBOM, signs with Cosign, and
  generates SLSA provenance (FR-015).

## Success criteria coverage

| Criterion | Validated by |
|-----------|--------------|
| SC-001 (local workflow passes, endpoints tested) | Step 1 |
| SC-002 (liveness < 100 ms, unauthenticated) | Step 2 |
| SC-003 (ready after startup; 503 before) | Step 2 + unit test |
| SC-004 (probes healthy in cluster) | Step 4 |
| SC-005 (not in rotation until ready) | Step 4 |
