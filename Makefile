# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

APP_NAME := lfx-v2-campaign-service
VERSION := $(shell git describe --tags --always)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_COMMIT := $(shell git rev-parse HEAD)

# Docker
DOCKER_REGISTRY := ghcr.io/linuxfoundation
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)/campaign-service
DOCKER_TAG ?= latest

# Helm variables
HELM_CHART_PATH=./charts/lfx-v2-campaign-service
HELM_RELEASE_NAME=lfx-v2-campaign-service
HELM_NAMESPACE=lfx
HELM_VALUES_FILE=$(HELM_CHART_PATH)/values.local.yaml

# Go
GO_VERSION := 1.24.2
GOOS := linux
GOARCH := amd64
# Go source files, excluding generated code under gen/.
GO_FILES := $(shell find . -type f -name '*.go' -not -path './gen/*')

# Linting
GOLANGCI_LINT_VERSION := v2.2.2
LINT_TIMEOUT := 10m
LINT_TOOL=$(shell go env GOPATH)/bin/golangci-lint

GOA_VERSION := v3.25.3

##@ Development

.PHONY: clean
clean: ## Remove temporary build artifacts (binaries, coverage)
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	rm -f coverage.out coverage-postgres.out

.PHONY: all
all: clean apigen fmt lint test build ## Clean, generate, format, lint, test, and build
	@echo "==> Ready for local testing: bin/$(APP_NAME)"

.PHONY: setup-dev
setup-dev: ## Setup development tools
	@echo "Installing development tools..."
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: setup
setup: ## Setup development environment
	@echo "Setting up development environment..."
	go mod download
	go mod tidy

.PHONY: deps
deps: ## Install dependencies
	@echo "Installing dependencies..."
	go install goa.design/goa/v3/cmd/goa@$(GOA_VERSION)

.PHONY: apigen
apigen: deps #@ Generate API code using Goa
	goa gen github.com/linuxfoundation/lfx-v2-campaign-service/design
	@echo "Copying OpenAPI specs into kodata for ko embedding..."
	@mkdir -p cmd/campaign-service/kodata/gen/http
	@cp gen/http/openapi.json gen/http/openapi.yaml gen/http/openapi3.json gen/http/openapi3.yaml cmd/campaign-service/kodata/gen/http/

.PHONY: fmt
fmt: ## Format Go code (gofmt + simplify)
	@echo "Formatting code..."
	@go fmt ./...
	@gofmt -s -w $(GO_FILES)

.PHONY: check-fmt
check-fmt: ## Verify Go code is formatted (fails if not); used in CI
	@echo "Checking code format..."
	@if [ -n "$$(gofmt -s -l $(GO_FILES))" ]; then \
		echo "The following files need formatting (run 'make fmt'):"; \
		gofmt -s -l $(GO_FILES); \
		exit 1; \
	fi
	@echo "==> Format OK"

.PHONY: lint
lint: ## Run golangci-lint (local Go linting)
	@echo "Running golangci-lint..."
	@which golangci-lint >/dev/null 2>&1 || (echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..." && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION))
	@golangci-lint run ./... && echo "==> Lint OK"

.PHONY: test
test: ## Run tests
	@echo "Running tests..."
	# internal/infrastructure/postgres and its dbtest subpackage each run live-database
	# migrations against the SAME TEST_DATABASE_URL from their own package-scoped
	# sync.Once (dbtest.Pool cannot be reused from the postgres package's own live tests —
	# it would be an import cycle, see audience_reconcile_live_test.go). `go test ./...`
	# builds and runs packages as separate binaries/processes, so with the default
	# parallelism these two can call Migrate() at the same moment. golang-migrate's
	# advisory lock serializes the two Up() calls, but 000018's CREATE INDEX CONCURRENTLY
	# still has to wait out every OTHER session's open transaction against the table —
	# including one held by the OTHER package's own live tests — which is exactly the
	# shape of Postgres's documented CONCURRENTLY-vs-concurrent-transaction deadlock.
	# `-p 1` forces every package under internal/infrastructure/postgres/... to run
	# strictly one at a time, closing the race at its source instead of retrying past it
	# (a retry after a failed CONCURRENTLY build risks finding the index already present
	# and INVALID under IF NOT EXISTS, which checkNoInvalidIndexes then fails permanently).
	go test -v -race -p 1 -coverprofile=coverage-postgres.out ./internal/infrastructure/postgres/...
	go test -v -race -coverprofile=coverage.out $$(go list ./... | grep -v -E '/internal/infrastructure/postgres(/|$$)')

.PHONY: build
build: ## Build the application for local OS
	@echo "Building application for local development..."
	go build \
		-ldflags "-X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME) -X main.gitCommit=$(GIT_COMMIT)" \
		-o bin/$(APP_NAME) ./cmd/campaign-service/

.PHONY: build-release
build-release: ## Build a static release binary for Linux (distinct from the container image)
	@echo "Building static release binary for $(GOOS)/$(GOARCH)..."
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-trimpath \
		-ldflags "-s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME) -X main.gitCommit=$(GIT_COMMIT)" \
		-o bin/$(APP_NAME) ./cmd/campaign-service/
	@echo "==> Release binary: bin/$(APP_NAME)"

.PHONY: run
run: build ## Run the application for local development
	@echo "Running application for local development..."
	./bin/$(APP_NAME)

##@ Docker

.PHONY: docker-build
docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest


.PHONY: docker-run
docker-run: ## Run Docker container locally
	@echo "Running Docker container..."
	docker run \
		--name $(APP_NAME) \
		-p 8080:8080 \
		-e OPENSEARCH_URL=http://opensearch-cluster-master.lfx.svc.cluster.local:9200 \
		-e NATS_URL=nats://lfx-platform-nats.lfx.svc.cluster.local:4222 \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

##@ Helm/Kubernetes
# Install Helm chart
.PHONY: helm-install
helm-install:
	@echo "==> Installing Helm chart..."
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE) --set image.tag=$(DOCKER_TAG)
	@echo "==> Helm chart installed: $(HELM_RELEASE_NAME)"

.PHONY: helm-install-local
helm-install-local: ## Install Helm chart with local values file
	@echo "==> Installing Helm chart with local values..."
	helm upgrade --force --install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
		--namespace $(HELM_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES_FILE)
	@echo "==> Helm chart installed: $(HELM_RELEASE_NAME)"


# Print templates for Helm chart
.PHONY: helm-templates
helm-templates:
	@echo "==> Printing templates for Helm chart..."
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE) --set image.tag=$(DOCKER_TAG)
	@echo "==> Templates printed for Helm chart: $(HELM_RELEASE_NAME)"

# Uninstall Helm chart
.PHONY: helm-uninstall
helm-uninstall:
	@echo "==> Uninstalling Helm chart..."
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_NAMESPACE)
	@echo "==> Helm chart uninstalled: $(HELM_RELEASE_NAME)"
