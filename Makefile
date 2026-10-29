# ------------------------------------------------------------------------------
# Vars
# ------------------------------------------------------------------------------

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

# KIND_CLUSTER_NAME is used to detect if tests are running locally via kind.
# Use a non-empty default for kind-based integration tests; override or clear
# it when running against an external cluster like OCP.
KIND_CLUSTER_NAME ?= coraza-kubernetes-operator-integration
# Test Gateways get metadata.labels["istio.io/rev"] when non-empty (see test/framework).
# Default coraza matches kind + operator Helm in hack/kind_cluster.py. For OpenShift,
# use openshift-gateway (match operator istio.revision) or empty for default-revision Istio.
ISTIO_GATEWAY_REVISION ?= coraza
ISTIO_VERSION ?= 1.31.1
# Pin kube-prometheus-stack chart version (https://artifacthub.io/packages/helm/prometheus-community/kube-prometheus-stack).
# Override at install time: make observability.prometheus.deploy KUBE_PROM_STACK_VERSION=x.y.z
KUBE_PROM_STACK_VERSION ?= 86.2.3
KUBE_PROM_STACK_RELEASE ?= kube-prometheus-stack
MONITORING_NAMESPACE ?= monitoring
DEMO_NAMESPACE ?= integration-tests
GRAFANA_ADMIN_PASSWORD ?= coraza-demo
# Matches Prometheus bundled in kube-prometheus-stack $(KUBE_PROM_STACK_VERSION).
# Used by promtool in CI and: make observability.promtool.install
PROM_VERSION ?= 3.12.0
PROM_SHA256_LINUX_AMD64 ?= 20da47f8e5303f74aecb78edd7f7e39041dac08ac4939dba75efd7a900ae8867
METALLB_VERSION ?= 0.16.1
METALLB_POOL_SIZE ?= 128 # Defines the size of MetalLB pool, when being used
# OpenTelemetry Operator chart used by make cluster.kind.otel.
OTEL_OPERATOR_VERSION ?= 0.114.0

VERSION ?= v0.0.0-dev
IMAGE_REGISTRY ?= ghcr.io/networking-incubator

HELM_RELEASE_NAME ?= coraza-kubernetes-operator
HELM_RELEASE_NAMESPACE ?= coraza-system
HELM_CHART_DIR ?= charts/coraza-kubernetes-operator
CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator
CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG ?= $(VERSION)
CONTROLLER_MANAGER_CONTAINER_IMAGE ?= ${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE}:${CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG}

# OCI image annotations (https://github.com/opencontainers/image-spec/blob/main/annotations.md)
GIT_REVISION = $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
OCI_IMAGE_CREATED = $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
OCI_IMAGE_SOURCE ?= https://github.com/networking-incubator/coraza-kubernetes-operator
OCI_IMAGE_DOCUMENTATION ?= $(OCI_IMAGE_SOURCE)
OCI_IMAGE_VENDOR ?= networking-incubator
OCI_IMAGE_LICENSES ?= Apache-2.0

# Must match the final stage in the root Dockerfile (distroless).
OCI_OPERATOR_BASE_NAME ?= gcr.io/distroless/static:nonroot
OCI_OPERATOR_BASE_DIGEST ?= sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39

OCI_IMAGE_TITLE_OPERATOR ?= Coraza Kubernetes Operator
OCI_IMAGE_DESC_OPERATOR ?= Kubernetes operator for Coraza Web Application Firewall on Gateway API.

OCI_IMAGE_TITLE_BUNDLE ?= Coraza Kubernetes Operator (OLM bundle)
OCI_IMAGE_DESC_BUNDLE ?= OLM bundle for the Coraza Kubernetes Operator.

OCI_IMAGE_TITLE_CATALOG ?= Coraza Kubernetes Operator (OLM catalog)
OCI_IMAGE_DESC_CATALOG ?= OLM index/catalog image for the Coraza Kubernetes Operator.

OPM_VERSION ?= v1.64.0
# Base image for the catalog final stage (see catalog/Dockerfile). When bumping OPM_VERSION, refresh
# OPM_BASE_DIGEST to the manifest list digest for that tag (e.g. quay.io API or skopeo inspect).
OPM_BASE_NAME ?= quay.io/operator-framework/opm:$(OPM_VERSION)
OPM_BASE_DIGEST ?= sha256:a070b901663d00312ccabe06a3c04b961d7326868499fe6c4c6b97025c79f014

OCI_LABELS_COMMON = \
	--label org.opencontainers.image.version="$(VERSION)" \
	--label org.opencontainers.image.revision="$(GIT_REVISION)" \
	--label org.opencontainers.image.created="$(OCI_IMAGE_CREATED)" \
	--label org.opencontainers.image.source="$(OCI_IMAGE_SOURCE)" \
	--label org.opencontainers.image.documentation="$(OCI_IMAGE_DOCUMENTATION)" \
	--label org.opencontainers.image.licenses="$(OCI_IMAGE_LICENSES)" \
	--label org.opencontainers.image.vendor="$(OCI_IMAGE_VENDOR)"

OCI_LABELS_OPERATOR = $(OCI_LABELS_COMMON) \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_OPERATOR)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_OPERATOR)" \
	--label org.opencontainers.image.base.name="$(OCI_OPERATOR_BASE_NAME)" \
	--label org.opencontainers.image.base.digest="$(OCI_OPERATOR_BASE_DIGEST)"

OCI_LABELS_BUNDLE = $(OCI_LABELS_COMMON) \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_BUNDLE)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_BUNDLE)"

OCI_LABELS_CATALOG = $(OCI_LABELS_COMMON) \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_CATALOG)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_CATALOG)" \
	--label org.opencontainers.image.base.name="$(OPM_BASE_NAME)" \
	--label org.opencontainers.image.base.digest="$(OPM_BASE_DIGEST)"

# ------------------------------------------------------------------------------
# General
# ------------------------------------------------------------------------------

.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9.-]+:.*?##/ { printf "  \033[36m%-40s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: print-%
print-%:
	@echo $($*)

.PHONY: all
all: build

# ------------------------------------------------------------------------------
# Build
# ------------------------------------------------------------------------------

# coraza.no_memoize: validation compiles untrusted rule text, and a failed parse
# returns a nil WAF that cannot release the memoizer it registered, so it leaks.
# The operator runs no WAF transactions, so nothing benefits from the cache.
.PHONY: build.binaries
build.binaries:
	go build -o bin/manager -tags 'no_fs_access coraza.no_memoize' ./cmd/manager
	go build -o bin/kubectl-coraza ./cmd/kubectl-coraza

.PHONY: build
build: manifests generate fmt vet lint build.binaries

.PHONY: build.image
build.image:
	$(CONTAINER_TOOL) build $(OCI_LABELS_OPERATOR) -t ${CONTROLLER_MANAGER_CONTAINER_IMAGE} .

.PHONY: build.installer
build.installer: manifests generate helm.sync ## Build a single install manifest (CRDs + operator)
	mkdir -p dist
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--include-crds \
		--set tls.generate=false \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG) \
		> dist/install.yaml
	@if grep -q '^  tls.key:' dist/install.yaml; then \
		echo "ERROR: dist/install.yaml contains a generated serving private key; create the serving Secret separately" >&2; \
		rm -f dist/install.yaml; \
		exit 1; \
	fi

.PHONY: release.manifests
release.manifests: manifests generate helm.sync ## Build release manifest bundles (crds, operator, samples)
	@echo "Building release manifest bundles..."
	@mkdir -p dist
	@echo "Building CRDs bundle..."
	cat $(HELM_CHART_DIR)/crds/*.yaml > dist/crds.yaml
	@echo "Building operator bundle..."
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--set tls.generate=false \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(VERSION) \
		> dist/operator.yaml
	@if grep -q '^  tls.key:' dist/operator.yaml; then \
		echo "ERROR: dist/operator.yaml contains a generated serving private key; create the serving Secret separately" >&2; \
		rm -f dist/operator.yaml; \
		exit 1; \
	fi
	@echo "Building samples bundle..."
	cat config/samples/gateway.yaml > dist/samples.yaml
	echo "---" >> dist/samples.yaml
	cat config/samples/ruleset.yaml >> dist/samples.yaml
	echo "---" >> dist/samples.yaml
	cat config/samples/engine.yaml >> dist/samples.yaml
	@echo "Packaging Helm chart..."
	helm package $(HELM_CHART_DIR) --version $(VERSION:v%=%) --app-version $(VERSION) --destination dist/
	@echo "Manifest bundles built successfully in dist/"
	@ls -lh dist/

.PHONY: release.kubectl-plugin
release.kubectl-plugin: ## Cross-build kubectl-coraza binaries into dist/ (linux/darwin, amd64/arm64)
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/kubectl-coraza-linux-amd64 ./cmd/kubectl-coraza
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/kubectl-coraza-linux-arm64 ./cmd/kubectl-coraza
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/kubectl-coraza-darwin-amd64 ./cmd/kubectl-coraza
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/kubectl-coraza-darwin-arm64 ./cmd/kubectl-coraza

.PHONY: release.operatorhub
release.operatorhub: ## Submit OLM bundle to OperatorHub community-operators
	hack/publish_operatorhub.sh --version $(VERSION:v%=%)

# ------------------------------------------------------------------------------
# Bundled WASM assets
# ------------------------------------------------------------------------------

ASSETS_DIR ?= internal/assets
# Raw plugin.wasm to bundle; required by wasm.bundle.
WASM_MODULE ?=
WASM_MODULE_NAME ?= envoy-coraza
# Optional manifest metadata (git SHA and linked versions of the real module).
WASM_GIT_SHA ?= $(CORAZA_RS_VERSION)
WASM_CORAZA_VERSION ?= 0.1.0
WASM_PROXY_WASM_SDK_VERSION ?= 6ceedbbdd07a048a65d197c9bbb1f950884561fb
# Embed the pinned OWASP CRS tree in the module (coraza-rs embed-crs feature).
# The release bundle does not embed CRS; the demo and wasm.crs do.
WASM_CRS ?= 0
WASM_CRS_VERSION ?= $(if $(filter 1,$(WASM_CRS)),$(or $(shell awk 'NR==1{print $$1}' '$(CORAZA_RS_DIR)/envoy-coraza/crs.pin' 2>/dev/null),dev),none)

.PHONY: wasm.bundle
wasm.bundle: ## Bundle WASM_MODULE=<path> (raw plugin.wasm) and rewrite its manifest
	@test -n "$(WASM_MODULE)" || { echo "WASM_MODULE is required: path to a raw plugin.wasm" >&2; exit 1; }
	@test -f "$(WASM_MODULE)" || { echo "WASM_MODULE not found: $(WASM_MODULE)" >&2; exit 1; }
	cp "$(WASM_MODULE)" "$(ASSETS_DIR)/plugin.wasm"
	@# Metadata is written as YAML plain scalars: git SHAs and versions are safe unquoted.
	@sum=$$(sha256sum "$(ASSETS_DIR)/plugin.wasm" | cut -d' ' -f1); \
		printf 'schema_version: 1\nmodule: %s\nfile: plugin.wasm\nsha256: %s\ngit_sha: %s\ncoraza_version: %s\nproxy_wasm_sdk_version: %s\ncrs_version: %s\n' \
			'$(WASM_MODULE_NAME)' "$$sum" '$(WASM_GIT_SHA)' '$(WASM_CORAZA_VERSION)' '$(WASM_PROXY_WASM_SDK_VERSION)' '$(WASM_CRS_VERSION)' \
			> "$(ASSETS_DIR)/plugin.manifest.yaml"; \
		echo "wasm.bundle: $$sum $(ASSETS_DIR)/plugin.wasm"

.PHONY: wasm.verify
wasm.verify: ## Verify the bundled module digest against plugin.manifest.yaml
	@want=$$(awk '/^sha256:/{print $$2; exit}' "$(ASSETS_DIR)/plugin.manifest.yaml"); \
	got=$$(sha256sum "$(ASSETS_DIR)/plugin.wasm" | cut -d' ' -f1); \
	if [ -z "$$want" ]; then echo "wasm.verify: no sha256 in $(ASSETS_DIR)/plugin.manifest.yaml" >&2; exit 1; fi; \
	if [ "$$want" != "$$got" ]; then echo "wasm.verify: digest mismatch: manifest $$want, $(ASSETS_DIR)/plugin.wasm $$got" >&2; exit 1; fi; \
	echo "wasm.verify: $$got $(ASSETS_DIR)/plugin.wasm ok"

# envoy-coraza built from coraza-rs. The default build has no embedded CRS;
# wasm.crs (and the demo) adds the pinned CRS tree. Point CORAZA_RS_DIR at a
# local checkout to build a branch without publishing anything:
#   make demo CORAZA_RS_DIR=/path/to/coraza-rs
# The build runs from an archive of CORAZA_RS_VERSION inside a temporary
# directory, so a user-provided checkout is read-only for this target (no clone,
# fetch, checkout, or new files).
#
# Istio 1.31 ships Envoy 1.39, which does not provide the WASI functions Rust
# std imports. The artifact is therefore linked with hack/wasi-stub.wat using
# the pinned Binaryen release; Envoy 1.40+ ignores the stubs.
CORAZA_RS_REPO ?= https://github.com/networking-incubator/coraza-rs
CORAZA_RS_VERSION ?= e1442ee3dc516a022a687a2bc9e239e39b629e03
CORAZA_RS_DIR ?= $(CURDIR)/.cache/coraza-rs
# Raw module produced by wasm.plugin (ignored by git, outside the source repo).
WASM_MODULE_OUT ?= $(CURDIR)/.cache/wasm/envoy_coraza.wasm
WASI_STUB ?= $(CURDIR)/hack/wasi-stub.wat
BINARYEN_VERSION ?= 123
BINARYEN_DIR ?= $(CURDIR)/.cache/binaryen

.PHONY: wasm.plugin
wasm.plugin: ## Build envoy-coraza from the pinned coraza-rs commit (skipped when WASM_MODULE is set)
	@set -e; \
	if [ -n "$(WASM_MODULE)" ]; then \
		echo "wasm.plugin: reusing $(WASM_MODULE)"; \
		exit 0; \
	fi; \
	default_dir="$(CURDIR)/.cache/coraza-rs"; \
	if [ "$(CORAZA_RS_DIR)" = "$$default_dir" ]; then \
		if [ ! -e "$$default_dir/.git" ]; then \
			echo "wasm.plugin: cloning $(CORAZA_RS_REPO)"; \
			git clone --quiet "$(CORAZA_RS_REPO)" "$$default_dir"; \
		fi; \
		if ! git -C "$$default_dir" rev-parse --verify --quiet "$(CORAZA_RS_VERSION)^{commit}" >/dev/null; then \
			echo "wasm.plugin: fetching $(CORAZA_RS_VERSION)"; \
			git -C "$$default_dir" fetch --quiet --all --tags; \
		fi; \
	fi; \
	if ! git -C "$(CORAZA_RS_DIR)" rev-parse --verify --quiet "$(CORAZA_RS_VERSION)^{commit}" >/dev/null; then \
		echo "wasm.plugin: pinned coraza-rs commit $(CORAZA_RS_VERSION) is not available in $(CORAZA_RS_REPO); pass CORAZA_RS_DIR=<checkout>" >&2; \
		exit 1; \
	fi; \
	work=$$(mktemp -d); \
	trap 'rm -rf "$$work"' EXIT; \
	git -C "$(CORAZA_RS_DIR)" archive --format=tar "$(CORAZA_RS_VERSION)" | tar -x -C "$$work"; \
	if [ "$(WASM_CRS)" = "1" ]; then \
		echo "wasm.plugin: fetching the pinned OWASP CRS"; \
		( cd "$$work" && ./envoy-coraza/crs-fetch.sh ); \
	fi; \
	echo "wasm.plugin: building $(CORAZA_RS_VERSION) with Rust"; \
	feat=""; \
	if [ "$(WASM_CRS)" = "1" ]; then feat="--features envoy-coraza/embed-crs"; fi; \
	( cd "$$work" && rustup target add wasm32-wasip1 >/dev/null && \
		cargo build --locked --release --target wasm32-wasip1 -p envoy-coraza $$feat ); \
	module="$$work/target/wasm32-wasip1/release/envoy_coraza.wasm"; \
	test -f "$$module" || { echo "wasm.plugin: build did not produce $$module" >&2; exit 1; }; \
	bin=$$(BINARYEN_VERSION="$(BINARYEN_VERSION)" BINARYEN_DIR="$(BINARYEN_DIR)" "$(CURDIR)/hack/install-binaryen.sh"); \
	"$$bin/wasm-as" "$(WASI_STUB)" -o "$$work/wasi-stub.wasm"; \
	mkdir -p "$(dir $(WASM_MODULE_OUT))"; \
	"$$bin/wasm-merge" "$$work/wasi-stub.wasm" wasi_snapshot_preview1 "$$module" envoy-coraza \
		-o "$(WASM_MODULE_OUT)" --rename-export-conflicts --all-features; \
	echo "wasm.plugin: $(WASM_MODULE_OUT)"

.PHONY: wasm.crs
wasm.crs: ## Build and bundle a CRS-enabled module (demo only; the release bundle omits CRS)
	$(MAKE) wasm.plugin WASM_CRS=1
	$(MAKE) wasm.bundle WASM_CRS=1 WASM_MODULE=$(WASM_MODULE_OUT)

# ------------------------------------------------------------------------------
# Deployment
# ------------------------------------------------------------------------------

HELM_RELEASE_NAME ?= coraza-kubernetes-operator
HELM_RELEASE_NAMESPACE ?= coraza-system
# Extra flags for helm upgrade --install (e.g. HELM_EXTRA_ARGS='--set foo=bar').
HELM_EXTRA_ARGS ?=
# KIND pre-creates coraza-system for Istio; do not let Helm manage that Namespace resource.
HELM_CREATE_NAMESPACE ?= false

.PHONY: install
install: deploy ## Alias for deploy (Helm installs CRDs and operator together)

.PHONY: uninstall
uninstall: undeploy ## Alias for undeploy

.PHONY: deploy
deploy: helm.sync ## Deploy operator into the cluster using Helm (dev logging enabled)
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--create-namespace \
		--set createNamespace=$(HELM_CREATE_NAMESPACE) \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG) \
		--set istio.revision=$(ISTIO_GATEWAY_REVISION) \
		--set logging.development=true \
		$(HELM_EXTRA_ARGS)

.PHONY: undeploy
undeploy: ## Remove operator from the cluster using Helm
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_RELEASE_NAMESPACE)

.PHONY: run
run: manifests generate fmt vet
	go run ./cmd/manager

# ------------------------------------------------------------------------------
# Development
# ------------------------------------------------------------------------------

.PHONY: manifests
manifests: controller-gen
	"$(CONTROLLER_GEN)" rbac:roleName=coraza-controller-manager crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint: golangci-lint
	"$(GOLANGCI_LINT)" run --build-tags integration ./...

.PHONY: lint.fix
lint.fix: golangci-lint
	"$(GOLANGCI_LINT)" run --fix --build-tags integration ./...

.PHONY: lint.config
lint.config: golangci-lint
	"$(GOLANGCI_LINT)" config verify

.PHONY: lint.api
lint.api: kube-api-linter
	"$(KUBE_API_LINTER)" run --config .kubeapilinter.yml ./...

# ------------------------------------------------------------------------------
# Test Cluster
# ------------------------------------------------------------------------------

.PHONY: cluster.kind
cluster.kind:
	ISTIO_VERSION=${ISTIO_VERSION} METALLB_VERSION=${METALLB_VERSION} METALLB_POOL_SIZE=${METALLB_POOL_SIZE} OTEL_OPERATOR_VERSION=${OTEL_OPERATOR_VERSION} CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE=${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE} CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG=${CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG} python3 hack/kind_cluster.py setup

# cluster.kind + OpenTelemetry Operator (central collector CRDs). Plain cluster.kind skips OTel.
.PHONY: cluster.kind.otel
cluster.kind.otel:
	INSTALL_OTEL_OPERATOR=true $(MAKE) cluster.kind
cluster.kind: ## Create the KIND test cluster
	ISTIO_VERSION=${ISTIO_VERSION} METALLB_VERSION=${METALLB_VERSION} METALLB_POOL_SIZE=${METALLB_POOL_SIZE} CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE=${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE} CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG=${CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG} python3 hack/kind_cluster.py setup --name ${KIND_CLUSTER_NAME}

.PHONY: cluster.load-images
cluster.load-images: ## Load the operator image into the KIND test cluster
	@$(CONTAINER_TOOL) exec ${KIND_CLUSTER_NAME}-control-plane crictl rmi ${CONTROLLER_MANAGER_CONTAINER_IMAGE} 2>/dev/null || true
	$(KIND) load docker-image ${CONTROLLER_MANAGER_CONTAINER_IMAGE} --name ${KIND_CLUSTER_NAME}

.PHONY: clean.cluster.kind
clean.cluster.kind: ## Delete the KIND test cluster
	python3 hack/kind_cluster.py delete --name ${KIND_CLUSTER_NAME}

# -------------------------------------------------------------------------------
# Testing
# -------------------------------------------------------------------------------

.PHONY: test
test: generate
	ISTIO_VERSION=${ISTIO_VERSION} go test -v ./...

.PHONY: test.coverage
test.coverage: generate
	@echo "Running tests with coverage..."
	@ISTIO_VERSION=${ISTIO_VERSION} go test -v ./... -coverprofile=coverage.out -covermode=atomic
	@echo "Coverage by package:"
	@go tool cover -func=coverage.out | grep -v "total:" || true
	@echo "Total coverage:"
	@total=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo "Total: $${total}%"

.PHONY: test.integration
test.integration:
	go clean -testcache
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} ISTIO_GATEWAY_REVISION=${ISTIO_GATEWAY_REVISION} go test -tags=integration ./test/integration/... -v

.PHONY: test.e2e
test.e2e:
	go clean -testcache
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} ISTIO_GATEWAY_REVISION=${ISTIO_GATEWAY_REVISION} go test -tags=e2e ./test/e2e/... -v $(TEST_ARGS)

.PHONY: test.tools
test.tools:
	cd tools/github_project_manager && go test -v ./...
	cd tools/grafana-dashboards && go test -v ./...


# -------------------------------------------------------------------------------
# Coraza Coreruleset targets
# -------------------------------------------------------------------------------

CORERULESET_VERSION ?= v4.28.0
LOCALRULES ?= $(CURDIR)/tmp/rules
CORERULESET_DIR ?= $(CURDIR)/tmp/coreruleset
TMP_DOWNLOAD_DIR ?= $(CURDIR)/tmp/download
NAMESPACE ?= default
CORERULESET_EXTRA_FLAGS ?=

$(LOCALRULES):
	mkdir -p "$(LOCALRULES)"

.PHONY: coraza.coreruleset.download
coraza.coreruleset.download:
	@echo "Downloading CoreRuleSet $(CORERULESET_VERSION)..."
	@rm -rf $(CORERULESET_DIR) $(TMP_DOWNLOAD_DIR)
	@mkdir -p $(TMP_DOWNLOAD_DIR)
	@curl -sSL https://github.com/coreruleset/coreruleset/archive/refs/tags/$(CORERULESET_VERSION).tar.gz | tar xz -C $(TMP_DOWNLOAD_DIR)
	@mkdir -p $(CORERULESET_DIR)
	@mv $(TMP_DOWNLOAD_DIR)/coreruleset-$(CORERULESET_VERSION:v%=%)/rules $(CORERULESET_DIR)/
	@mkdir -p $(CORERULESET_DIR)/tests
	@mv $(TMP_DOWNLOAD_DIR)/coreruleset-$(CORERULESET_VERSION:v%=%)/tests/regression/tests $(CORERULESET_DIR)/tests/
	@rm -rf $(TMP_DOWNLOAD_DIR)
	@echo "CoreRuleSet extracted to $(CORERULESET_DIR)"

.PHONY: coraza.generaterules
coraza.generaterules: coraza.coreruleset.download $(LOCALRULES)
	@go run ./cmd/kubectl-coraza generate coreruleset --rules-dir $(CORERULESET_DIR)/rules/ --version $(CORERULESET_VERSION:v%=%) $(CORERULESET_EXTRA_FLAGS) > $(LOCALRULES)/rules.yaml

.PHONY: coraza.coreruleset
coraza.coreruleset: coraza.generaterules
	kubectl delete -n $(NAMESPACE) --ignore-not-found -f $(LOCALRULES)/*.yaml
	kubectl apply -n $(NAMESPACE) --server-side -f $(LOCALRULES)/*.yaml

# -------------------------------------------------------------------------------
# Coraza Coreruleset - Conformance test
# -------------------------------------------------------------------------------
CONFORMANCE_EXTRA_FLAGS ?=
FTW_CONFIG ?= $(CURDIR)/test/conformance/ftw.yml
FTW_OVERRIDES ?= $(CURDIR)/test/conformance/.ftw-overrides.yml

# Verifies generator output for pinned CRS (CORERULESET_VERSION + --include-test-rule + full CRS for parity) against tools/corerulesetgen/testdata/coreruleset_parity.sha256.
# Conformance needs --ignore-unsupported-rules=none so output matches the pre-exclusion golden hash and FTW exercises the full rule set.
.PHONY: coreruleset.verify-parity
coreruleset.verify-parity:
	@$(MAKE) CORERULESET_EXTRA_FLAGS="--include-test-rule --ignore-unsupported-rules=none --skip-validation-rulesource=request-999-common-exceptions-after" coraza.generaterules
	sha256sum -c tools/corerulesetgen/testdata/coreruleset_parity.sha256

.PHONY: test.conformance
test.conformance: coreruleset.verify-parity
	cd test/conformance && \
		$(CONFORMANCE_EXTRA_FLAGS) \
		FTW_CONFIG=$(FTW_CONFIG) \
		FTW_OVERRIDES=$(FTW_OVERRIDES) \
		TESTMANIFESTS_PATH=$(CORERULESET_DIR)/tests/tests \
		RULESET_PATH=$(LOCALRULES)/rules.yaml \
		KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME} \
		ISTIO_VERSION=${ISTIO_VERSION} \
		ISTIO_GATEWAY_REVISION=${ISTIO_GATEWAY_REVISION} \
		go test -tags=conformance ./... -v

# -------------------------------------------------------------------------------
# OLM Bundle
# -------------------------------------------------------------------------------

BUNDLE_IMG_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator-bundle
BUNDLE_IMG_TAG ?= $(VERSION)
BUNDLE_IMG ?= $(BUNDLE_IMG_BASE):$(BUNDLE_IMG_TAG)

CATALOG_IMG_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator-catalog
CATALOG_IMG_TAG ?= $(VERSION)
CATALOG_IMG ?= $(CATALOG_IMG_BASE):$(CATALOG_IMG_TAG)
BUNDLE_DIR ?= bundle
CATALOG_DIR ?= catalog
CATALOG_FILE ?= $(CATALOG_DIR)/coraza-kubernetes-operator/catalog.yaml
OLM_CHANNEL ?= alpha
# Bare semver (no v). Keep in sync with DEFAULT_MIN_KUBE_VERSION in hack/generate_bundle.py.
KUBE_VERSION ?= 1.32.0

.PHONY: bundle
bundle: helm.sync ## Generate OLM bundle from Helm chart
	python3 hack/generate_bundle.py \
		--chart-dir $(HELM_CHART_DIR) \
		--bundle-dir $(BUNDLE_DIR) \
		--version $(VERSION) \
		--image $(CONTROLLER_MANAGER_CONTAINER_IMAGE) \
		--channels $(OLM_CHANNEL) \
		--default-channel $(OLM_CHANNEL) \
		--min-kube-version $(KUBE_VERSION)

.PHONY: bundle.opp
bundle.opp: bundle ## Run OPP kiwi tests against staged bundle (needs docker, ansible, jmespath)
	KIND_KUBE_VERSION=v$(KUBE_VERSION) OPERATOR_VERSION=$(VERSION:v%=%) ./hack/operatorhub_opp_test.sh

.PHONY: bundle.build
bundle.build: ## Build the OLM bundle image
	$(CONTAINER_TOOL) build $(OCI_LABELS_BUNDLE) -f $(BUNDLE_DIR)/bundle.Dockerfile -t $(BUNDLE_IMG) $(BUNDLE_DIR)

.PHONY: bundle.push
bundle.push: ## Push the OLM bundle image
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

.PHONY: catalog.update
catalog.update: ## Add the current VERSION to the OLM catalog channel
	python3 hack/update_catalog.py $(CATALOG_FILE) $(VERSION) coraza-kubernetes-operator $(OLM_CHANNEL)

.PHONY: catalog.build
catalog.build: ## Build the OLM catalog image (renders bundles via opm)
	@rm -rf $(CATALOG_DIR)/bundles && mkdir -p $(CATALOG_DIR)/bundles
	@if [ -z "$${BUNDLE_IMGS}" ] && [ -d "$(BUNDLE_DIR)/manifests" ]; then \
		cp -r $(BUNDLE_DIR) $(CATALOG_DIR)/bundles/local; \
	fi
	@if [ -n "$${BUNDLE_IMGS}" ]; then \
		bundle_imgs="$${BUNDLE_IMGS}"; \
	elif [ -d "$(CATALOG_DIR)/bundles/local" ]; then \
		bundle_imgs="/tmp/bundles/local"; \
	else \
		bundle_imgs=$$(python3 hack/list_bundle_images.py $(CATALOG_FILE) $(BUNDLE_IMG_BASE)); \
	fi && \
	$(CONTAINER_TOOL) build \
		$(OCI_LABELS_CATALOG) \
		--build-arg OPM_VERSION=$(OPM_VERSION) \
		--build-arg BUNDLE_IMGS="$$bundle_imgs" \
		-f $(CATALOG_DIR)/Dockerfile -t $(CATALOG_IMG) $(CATALOG_DIR)
	@rm -rf $(CATALOG_DIR)/bundles

.PHONY: catalog.push
catalog.push: ## Push the OLM catalog image
	$(CONTAINER_TOOL) push $(CATALOG_IMG)

CATALOG_NAMESPACE ?= olm

.PHONY: catalog.deploy
catalog.deploy: ## Deploy the CatalogSource CR to the cluster
	@printf '%s\n' \
	  'apiVersion: operators.coreos.com/v1alpha1' \
	  'kind: CatalogSource' \
	  'metadata:' \
	  '  name: coraza-kubernetes-operator' \
	  '  namespace: $(CATALOG_NAMESPACE)' \
	  'spec:' \
	  '  sourceType: grpc' \
	  '  image: $(CATALOG_IMG)' \
	  '  displayName: Coraza Kubernetes Operator' \
	  '  updateStrategy:' \
	  '    registryPoll:' \
	  '      interval: 10m' \
	  | kubectl apply -f -
	kubectl -n $(CATALOG_NAMESPACE) wait --for=jsonpath='{.status.connectionState.lastObservedState}'=READY catalogsource/coraza-kubernetes-operator --timeout=60s

.PHONY: catalog.undeploy
catalog.undeploy: ## Remove the CatalogSource CR from the cluster
	kubectl delete catalogsource coraza-kubernetes-operator -n $(CATALOG_NAMESPACE) --ignore-not-found

# -------------------------------------------------------------------------------
# Helm
# -------------------------------------------------------------------------------

.PHONY: helm.lint
helm.lint: ## Lint the Helm chart
	helm lint $(HELM_CHART_DIR)

.PHONY: helm.template
helm.template: ## Render the Helm chart templates locally
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) --namespace $(HELM_RELEASE_NAMESPACE)

.PHONY: helm.sync-crds
helm.sync-crds: manifests ## Copy generated CRDs into the Helm chart
	cp config/crd/bases/*.yaml $(HELM_CHART_DIR)/crds/

.PHONY: helm.sync-rbac
helm.sync-rbac: manifests ## Sync generated RBAC rules into the Helm chart ClusterRole and Role
	@GEN=config/rbac/role.yaml; \
	CR_CHART=$(HELM_CHART_DIR)/templates/clusterrole.yaml; \
	sed '/^rules:/q' "$$CR_CHART" > "$$CR_CHART.tmp" && \
	awk '/^kind: ClusterRole/{found=1} found && /^rules:/{start=1; next} start && /^---/{exit} start{print}' "$$GEN" >> "$$CR_CHART.tmp" && \
	mv "$$CR_CHART.tmp" "$$CR_CHART"; \
	R_CHART=$(HELM_CHART_DIR)/templates/role.yaml; \
	sed '/^rules:/q' "$$R_CHART" > "$$R_CHART.tmp" && \
	awk '/^kind: Role/{found=1} found && /^rules:/{start=1; next} start && /^---/{exit} start{print}' "$$GEN" >> "$$R_CHART.tmp" && \
	mv "$$R_CHART.tmp" "$$R_CHART"

.PHONY: helm.sync
helm.sync: helm.sync-crds helm.sync-rbac ## Sync all generated resources into the Helm chart

# -------------------------------------------------------------------------------
# Testing - Helm
# -------------------------------------------------------------------------------

.PHONY: helm.validate
helm.validate: ## Validate Helm templates render correctly for key flag combinations
	hack/test-helm-manifests.sh

.PHONY: test.metrics
test.metrics: ## Run metrics integration tests (requires KIND cluster)
	go clean -testcache
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} go test -tags=integration -run TestCorazaControllerMetrics ./test/integration/... -v

# -------------------------------------------------------------------------------
# Observability demo (KIND + kube-prometheus-stack + Grafana dashboards)
# -------------------------------------------------------------------------------

.PHONY: observability.dashboard.generate
observability.dashboard.generate: ## Regenerate Grafana dashboard JSON via Go Foundation SDK
	cd tools/grafana-dashboards && go run ./cmd/generate

.PHONY: observability.dashboard.test
observability.dashboard.test: ## Run grafana-dashboards unit and golden tests
	cd tools/grafana-dashboards && go test ./...

.PHONY: observability.dashboard.validate
observability.dashboard.validate: observability.dashboard.test ## Go tests + lint committed chart dashboard JSON
	hack/observability/validate-dashboards.sh

.PHONY: observability.promtool.install
observability.promtool.install: ## Install promtool (PROM_VERSION) to INSTALL_DIR (default: GOBIN)
	PROM_VERSION=$(PROM_VERSION) PROM_SHA256=$(PROM_SHA256_LINUX_AMD64) \
		INSTALL_DIR=$(or $(INSTALL_DIR),$(GOBIN)) \
		hack/observability/install-promtool.sh

.PHONY: observability.prometheus.deploy
observability.prometheus.deploy: ## Install kube-prometheus-stack on the KIND cluster
	@set -e; \
	ctx="kind-$(KIND_CLUSTER_NAME)"; \
	cfg="$(CURDIR)/config/observability"; \
	kubectl --context "$$ctx" cluster-info >/dev/null; \
	echo "=== Installing $(KUBE_PROM_STACK_RELEASE) ($(KUBE_PROM_STACK_VERSION)) ==="; \
	helm --kube-context "$$ctx" repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true; \
	helm --kube-context "$$ctx" repo update prometheus-community; \
	kubectl --context "$$ctx" create namespace "$(MONITORING_NAMESPACE)" 2>/dev/null || true; \
	helm --kube-context "$$ctx" upgrade --install "$(KUBE_PROM_STACK_RELEASE)" prometheus-community/kube-prometheus-stack \
		--namespace "$(MONITORING_NAMESPACE)" \
		--version "$(KUBE_PROM_STACK_VERSION)" \
		--values "$$cfg/kube-prometheus-stack-values.yaml" \
		--set-string grafana.adminPassword="$(GRAFANA_ADMIN_PASSWORD)" \
		--wait --timeout 10m; \
	kubectl --context "$$ctx" -n "$(MONITORING_NAMESPACE)" wait --for=condition=Available \
		deployment/"$(KUBE_PROM_STACK_RELEASE)-operator" --timeout=300s; \
	kubectl --context "$$ctx" -n "$(MONITORING_NAMESPACE)" wait --for=condition=Ready \
		pod -l app.kubernetes.io/name=prometheus --timeout=300s; \
	kubectl --context "$$ctx" -n "$(MONITORING_NAMESPACE)" wait --for=condition=Ready \
		pod -l app.kubernetes.io/name=grafana --timeout=300s; \
	sed -e "s/PLACEHOLDER_PROMETHEUS_SA/$(KUBE_PROM_STACK_RELEASE)-prometheus/g" \
		-e "s/PLACEHOLDER_MONITORING_NS/$(MONITORING_NAMESPACE)/g" \
		"$$cfg/prometheus-rbac.yaml" | kubectl --context "$$ctx" apply -f -

.PHONY: observability.prometheus.undeploy
observability.prometheus.undeploy: ## Remove kube-prometheus-stack from the KIND cluster
	@set -e; \
	ctx="kind-$(KIND_CLUSTER_NAME)"; \
	if kubectl --context "$$ctx" cluster-info >/dev/null 2>&1; then \
		helm --kube-context "$$ctx" uninstall "$(KUBE_PROM_STACK_RELEASE)" --namespace "$(MONITORING_NAMESPACE)" 2>/dev/null || true; \
		kubectl --context "$$ctx" delete clusterrolebinding coraza-metrics-reader-prometheus --ignore-not-found; \
		kubectl --context "$$ctx" delete clusterrole coraza-metrics-reader --ignore-not-found; \
		kubectl --context "$$ctx" delete namespace "$(MONITORING_NAMESPACE)" --ignore-not-found --timeout=120s || true; \
	fi

.PHONY: observability.operator.monitoring
observability.operator.monitoring: ## Enable ServiceMonitor, PrometheusRule, and Grafana dashboards on the operator
	@set -e; \
	ctx="kind-$(KIND_CLUSTER_NAME)"; \
	kubectl --context "$$ctx" cluster-info >/dev/null; \
	helm_extra="--reuse-values"; \
	if ! helm --kube-context "$$ctx" status "$(HELM_RELEASE_NAME)" -n "$(HELM_RELEASE_NAMESPACE)" &>/dev/null; then \
		helm_extra=""; \
	fi; \
	echo "=== Enabling operator monitoring ==="; \
	helm --kube-context "$$ctx" upgrade --install "$(HELM_RELEASE_NAME)" "$(HELM_CHART_DIR)" \
		--namespace "$(HELM_RELEASE_NAMESPACE)" \
		--create-namespace \
		$$helm_extra \
		--set metrics.serviceMonitor.enabled=true \
		--set metrics.serviceMonitor.additionalLabels.release="$(KUBE_PROM_STACK_RELEASE)" \
		--set metrics.prometheusRule.enabled=true \
		--set metrics.prometheusRule.additionalLabels.release="$(KUBE_PROM_STACK_RELEASE)" \
		--set metrics.grafanaDashboard.enabled=true \
		--wait --timeout 5m; \
	kubectl --context "$$ctx" -n "$(HELM_RELEASE_NAMESPACE)" wait --for=condition=Available \
		deployment/"$(HELM_RELEASE_NAME)" --timeout=300s

# Server-side apply with forced conflicts keeps the demo idempotent after a
# manual edit of a sample object.
.PHONY: demo.workload
demo.workload: ## Apply the demo CRs (config/samples) and wait for acceptance
	@set -e; \
	ctx="kind-$(KIND_CLUSTER_NAME)"; \
	kubectl --context "$$ctx" cluster-info >/dev/null; \
	echo "=== Applying demo workload (config/samples) to $(DEMO_NAMESPACE) ==="; \
	kubectl --context "$$ctx" create namespace "$(DEMO_NAMESPACE)" 2>/dev/null || true; \
	kubectl --context "$$ctx" apply --server-side --force-conflicts \
		-n "$(DEMO_NAMESPACE)" -k "$(CURDIR)/config/samples"; \
	kubectl --context "$$ctx" -n "$(DEMO_NAMESPACE)" wait --for=condition=Available \
		deployment/echo --timeout=300s; \
	echo "=== Waiting for Engine Ready ==="; \
	ready=""; \
	deadline=$$(($$(date +%s) + 300)); \
	while [ "$$(date +%s)" -lt "$$deadline" ]; do \
		ready=$$(kubectl --context "$$ctx" -n "$(DEMO_NAMESPACE)" get engine coraza \
			-o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true); \
		if [ "$$ready" = "True" ]; then echo "Engine coraza is Ready"; break; fi; \
		sleep 5; \
	done; \
	if [ "$$ready" != "True" ]; then echo "ERROR: Engine coraza not Ready within timeout" >&2; exit 1; fi; \
	echo "=== Waiting for RuleSet Ready ==="; \
	if ! kubectl --context "$$ctx" -n "$(DEMO_NAMESPACE)" wait --for=condition=Ready \
		ruleset/default-ruleset --timeout=300s 2>/dev/null; then \
		echo "ERROR: RuleSet default-ruleset not Ready within timeout" >&2; exit 1; \
	fi

.PHONY: observability.demo.workload
observability.demo.workload: demo.workload ## Apply demo CRs and seed traffic for dashboard metrics
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) DEMO_NAMESPACE=$(DEMO_NAMESPACE) \
		hack/observability/seed-traffic.sh

.PHONY: observability.demo
observability.demo: observability.prometheus.deploy observability.operator.monitoring observability.demo.workload observability.grafana.url ## Full observability demo on existing KIND cluster

.PHONY: demo
demo: ## Single command: build the CRS-enabled WASM module, create KIND, deploy the operator and the demo rules
	$(MAKE) wasm.crs
	$(MAKE) wasm.verify
	$(MAKE) cluster.kind
	@# The chart reuses imagePullPolicy IfNotPresent, so restart the operator to run the image just loaded.
	kubectl --context kind-$(KIND_CLUSTER_NAME) -n $(HELM_RELEASE_NAMESPACE) rollout restart deployment/$(HELM_RELEASE_NAME)
	$(MAKE) demo.workload
	$(MAKE) demo.verify
	@echo "note: $(ASSETS_DIR)/plugin.wasm now holds the built module; restore the committed fixture with: git checkout $(ASSETS_DIR)"

.PHONY: demo.verify
demo.verify: ## Assert the demo Gateway passes normal traffic and blocks the demo attack
	@set -e; \
	ctx="kind-$(KIND_CLUSTER_NAME)"; \
	addr=""; \
	deadline=$$(($$(date +%s) + 300)); \
	while [ "$$(date +%s)" -lt "$$deadline" ]; do \
		addr=$$(kubectl --context "$$ctx" -n "$(DEMO_NAMESPACE)" get gateway coraza-gateway \
			-o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true); \
		[ -n "$$addr" ] && break; \
		sleep 5; \
	done; \
	[ -n "$$addr" ] || { echo "ERROR: Gateway coraza-gateway has no address" >&2; exit 1; }; \
	echo "=== Sending demo traffic to http://$$addr/ ==="; \
	allowed=""; blocked=""; data=""; \
	deadline=$$(($$(date +%s) + 240)); \
	while [ "$$(date +%s)" -lt "$$deadline" ]; do \
		allowed=$$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$$addr/" || true); \
		blocked=$$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$$addr/?q=evilmonkey" || true); \
		data=$$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$$addr/?q=evildata" || true); \
		[ "$$allowed" = "200" ] && [ "$$blocked" = "403" ] && [ "$$data" = "403" ] && break; \
		sleep 5; \
	done; \
	echo "  allowed  GET /                      -> $$allowed (want 200)"; \
	echo "  blocked  GET /?q=evilmonkey         -> $$blocked (want 403)"; \
	echo "  blocked  GET /?q=evildata           -> $$data (want 403, @pmFromFile RuleData)"; \
	[ "$$allowed" = "200" ] && [ "$$blocked" = "403" ] && [ "$$data" = "403" ] || { \
		echo "ERROR: the WAF is not enforcing (is the WASM module loaded?)" >&2; exit 1; }; \
	echo "=== Demo verified: rules from the cluster block the request ==="


.PHONY: observability.grafana.port-forward
observability.grafana.port-forward: ## Port-forward Grafana to http://localhost:3000
	@echo "Grafana: http://localhost:3000 (admin / $(GRAFANA_ADMIN_PASSWORD))"
	@echo "Press Ctrl+C to stop."
	kubectl --context kind-$(KIND_CLUSTER_NAME) -n $(MONITORING_NAMESPACE) \
		port-forward svc/$(KUBE_PROM_STACK_RELEASE)-grafana 3000:80

.PHONY: observability.grafana.url
observability.grafana.url: ## Print Grafana URL, credentials, and dashboard links
	@echo "Grafana (port-forward):  make observability.grafana.port-forward"
	@echo "URL:      http://localhost:3000"
	@echo "User:     admin"
	@echo "Password: $(GRAFANA_ADMIN_PASSWORD)"
	@echo "Dashboards (folder: Coraza WAF):"
	@echo "  - Coraza Operator — Overview:   /d/coraza-operator-overview"
	@echo "  - Coraza Operator — Resources:  /d/coraza-operator-resources"

# -------------------------------------------------------------------------------
# Documentation
# -------------------------------------------------------------------------------

DOCS_DIR = docs
DOCS_BASE_URL ?= https://networking-incubator.github.io/coraza-kubernetes-operator/
DOCS_IMG = coraza-kubernetes-operator-docs

.PHONY: docs.image
docs.image: ## Build the documentation container image
	$(CONTAINER_TOOL) build -t $(DOCS_IMG) $(DOCS_DIR)

.PHONY: docs.serve
docs.serve: docs.image ## Serve documentation locally with live reload
	$(CONTAINER_TOOL) run --rm -it \
		-v $(CURDIR)/$(DOCS_DIR):/src:z -p 1313:1313 $(DOCS_IMG) \
		hugo server --bind 0.0.0.0 --baseURL http://localhost:1313/ --buildDrafts --buildFuture

.PHONY: docs.api
docs.api: docs.image ## Generate CRD API reference from Go types
	$(CONTAINER_TOOL) run --rm --user 0:0 \
		-v $(CURDIR):/repo:z -w /repo $(DOCS_IMG) \
		sh -c 'crd-ref-docs \
			--source-path=api \
			--config=hack/crd-ref-templates/config.yaml \
			--templates-dir=hack/crd-ref-templates \
			--renderer=markdown \
			--output-path=$(DOCS_DIR)/content/reference/api.md \
		&& chown $(shell id -u):$(shell id -g) $(DOCS_DIR)/content/reference/api.md'

.PHONY: docs.build
docs.build: docs.image ## Build multi-version documentation for production
	CONTAINER_TOOL=$(CONTAINER_TOOL) DOCS_IMG=$(DOCS_IMG) \
		hack/build-versioned-docs.sh --base-url "$(DOCS_BASE_URL)"

.PHONY: docs.serve.versioned
docs.serve.versioned: ## Build and serve multi-version site via static file server
	$(MAKE) DOCS_BASE_URL="http://localhost:1313" docs.build
	@echo "Serving full multi-version site on http://localhost:1313/"
	python3 -m http.server 1313 --directory docs/public

CHROMA_LIGHT_STYLE ?= tango
CHROMA_DARK_STYLE  ?= dracula

.PHONY: docs.chroma
docs.chroma: docs.image ## Regenerate Chroma syntax highlighting CSS for light and dark modes
	$(CONTAINER_TOOL) run --rm --user 0:0 -v $(CURDIR)/$(DOCS_DIR):/src:z $(DOCS_IMG) \
		sh -c 'hugo gen chromastyles --style=$(CHROMA_LIGHT_STYLE) > /src/assets/scss/_chroma_light.css \
		&& hugo gen chromastyles --style=$(CHROMA_DARK_STYLE) > /src/assets/scss/_chroma_dark.css \
		&& chown $(shell id -u):$(shell id -g) /src/assets/scss/_chroma_light.css /src/assets/scss/_chroma_dark.css'
	@echo "Generated light ($(CHROMA_LIGHT_STYLE)) and dark ($(CHROMA_DARK_STYLE)) Chroma stylesheets."
	@echo "Copy the rules into $(DOCS_DIR)/assets/scss/_styles_project.scss:"
	@echo "  - Light: top-level (replace 'Light mode syntax highlighting' section)"
	@echo "  - Dark: inside [data-bs-theme=\"dark\"] (replace 'Dark mode syntax highlighting' section)"
	@rm -f $(DOCS_DIR)/assets/scss/_chroma_light.css $(DOCS_DIR)/assets/scss/_chroma_dark.css

# -------------------------------------------------------------------------------
# Dependencies
# -------------------------------------------------------------------------------

LOCALBIN ?= $(CURDIR)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL ?= kubectl
KIND ?= kind
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
KUBE_API_LINTER = $(LOCALBIN)/golangci-lint-kube-api-linter

CONTROLLER_TOOLS_VERSION ?= v0.21.0
GOLANGCI_LINT_VERSION ?= v2.15.0
OPERATOR_SDK_VERSION ?= v1.42.0
KUBE_API_LINTER_VERSION ?= v0.0.0-20261012124120-79b557aa8c8d

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint $(GOLANGCI_LINT)
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK)
$(OPERATOR_SDK): $(LOCALBIN)
	@[ -f "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)" ] || { \
	set -e; \
	os=$$(go env GOOS); arch=$$(go env GOARCH); \
	url=https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION); \
	echo "Downloading operator-sdk $(OPERATOR_SDK_VERSION)"; \
	curl -fsSLo "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)" "$${url}/operator-sdk_$${os}_$${arch}"; \
	curl -fsSLo /tmp/operator-sdk-checksums.txt "$${url}/checksums.txt"; \
	grep "operator-sdk_$${os}_$${arch}$$" /tmp/operator-sdk-checksums.txt \
		| sed "s|operator-sdk_$${os}_$${arch}|$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)|" \
		| sha256sum -c -; \
	chmod +x "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)"; \
	}
	@ln -sf "$$(realpath "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)")" "$(OPERATOR_SDK)"

.PHONY: kube-api-linter
kube-api-linter: $(KUBE_API_LINTER)
$(KUBE_API_LINTER): $(LOCALBIN)
	$(call go-install-tool,$(KUBE_API_LINTER),sigs.k8s.io/kube-api-linter/cmd/golangci-lint-kube-api-linter,$(KUBE_API_LINTER_VERSION))

define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
n=0 ;\
until GOBIN="$(LOCALBIN)" go install $${package}; do \
	n=$$((n+1)) ;\
	if [ $$n -ge 3 ]; then \
		echo "go install $${package} failed after 3 attempts" ;\
		exit 1 ;\
	fi ;\
	echo "go install $${package} failed (attempt $$n/3), retrying..." ;\
	sleep $$((n*2)) ;\
done ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
