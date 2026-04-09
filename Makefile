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
ISTIO_VERSION ?= 1.28.2
METALLB_VERSION ?= 0.15.3
METALLB_POOL_SIZE ?= 128 # Defines the size of MetalLB pool, when being used

VERSION ?= v0.0.0-dev
IMAGE_REGISTRY ?= ghcr.io/networking-incubator
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

OCI_LABELS_OPERATOR = \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_OPERATOR)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_OPERATOR)" \
	--label org.opencontainers.image.version="$(VERSION)" \
	--label org.opencontainers.image.revision="$(GIT_REVISION)" \
	--label org.opencontainers.image.created="$(OCI_IMAGE_CREATED)" \
	--label org.opencontainers.image.source="$(OCI_IMAGE_SOURCE)" \
	--label org.opencontainers.image.documentation="$(OCI_IMAGE_DOCUMENTATION)" \
	--label org.opencontainers.image.licenses="$(OCI_IMAGE_LICENSES)" \
	--label org.opencontainers.image.vendor="$(OCI_IMAGE_VENDOR)" \
	--label org.opencontainers.image.base.name="$(OCI_OPERATOR_BASE_NAME)" \
	--label org.opencontainers.image.base.digest="$(OCI_OPERATOR_BASE_DIGEST)"

OCI_LABELS_BUNDLE = \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_BUNDLE)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_BUNDLE)" \
	--label org.opencontainers.image.version="$(VERSION)" \
	--label org.opencontainers.image.revision="$(GIT_REVISION)" \
	--label org.opencontainers.image.created="$(OCI_IMAGE_CREATED)" \
	--label org.opencontainers.image.source="$(OCI_IMAGE_SOURCE)" \
	--label org.opencontainers.image.documentation="$(OCI_IMAGE_DOCUMENTATION)" \
	--label org.opencontainers.image.licenses="$(OCI_IMAGE_LICENSES)" \
	--label org.opencontainers.image.vendor="$(OCI_IMAGE_VENDOR)"

OCI_LABELS_CATALOG = \
	--label org.opencontainers.image.title="$(OCI_IMAGE_TITLE_CATALOG)" \
	--label org.opencontainers.image.description="$(OCI_IMAGE_DESC_CATALOG)" \
	--label org.opencontainers.image.version="$(VERSION)" \
	--label org.opencontainers.image.revision="$(GIT_REVISION)" \
	--label org.opencontainers.image.created="$(OCI_IMAGE_CREATED)" \
	--label org.opencontainers.image.source="$(OCI_IMAGE_SOURCE)" \
	--label org.opencontainers.image.documentation="$(OCI_IMAGE_DOCUMENTATION)" \
	--label org.opencontainers.image.licenses="$(OCI_IMAGE_LICENSES)" \
	--label org.opencontainers.image.vendor="$(OCI_IMAGE_VENDOR)" \
	--label org.opencontainers.image.base.name="$(OPM_BASE_NAME)" \
	--label org.opencontainers.image.base.digest="$(OPM_BASE_DIGEST)"

# ------------------------------------------------------------------------------
# General
# ------------------------------------------------------------------------------

.PHONY: help
help: ## Show this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9.-]+:.*?##/ { printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: all
all: build

# ------------------------------------------------------------------------------
# Build
# ------------------------------------------------------------------------------

.PHONY: build
build: manifests generate fmt vet lint
	go build -o bin/manager -tags no_fs_access ./cmd/manager
	go build -o bin/kubectl-coraza ./cmd/kubectl-coraza

.PHONY: build.image
build.image:
	$(CONTAINER_TOOL) build $(OCI_LABELS_OPERATOR) -t ${CONTROLLER_MANAGER_CONTAINER_IMAGE} .

.PHONY: build.installer
build.installer: manifests generate helm.sync ## Build a single install manifest (CRDs + operator)
	mkdir -p dist
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--include-crds \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG) \
		> dist/install.yaml

.PHONY: release.manifests
release.manifests: manifests generate helm.sync ## Build release manifest bundles (crds, operator, samples)
	@echo "Building release manifest bundles..."
	@mkdir -p dist
	@echo "Building CRDs bundle..."
	cat $(HELM_CHART_DIR)/crds/*.yaml > dist/crds.yaml
	@echo "Building operator bundle..."
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(VERSION) \
		> dist/operator.yaml
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
# Deployment
# ------------------------------------------------------------------------------

HELM_RELEASE_NAME ?= coraza-kubernetes-operator
HELM_RELEASE_NAMESPACE ?= coraza-system

.PHONY: install
install: deploy ## Alias for deploy (Helm installs CRDs and operator together)

.PHONY: uninstall
uninstall: undeploy ## Alias for undeploy

.PHONY: deploy
deploy: helm.sync ## Deploy operator into the cluster using Helm (dev logging enabled)
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--create-namespace \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG) \
		--set istio.revision=$(ISTIO_GATEWAY_REVISION) \
		--set logging.development=true

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
	"$(CONTROLLER_GEN)" rbac:roleName=coraza-controller-manager crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

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
	ISTIO_VERSION=${ISTIO_VERSION} METALLB_VERSION=${METALLB_VERSION} METALLB_POOL_SIZE=${METALLB_POOL_SIZE} CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE=${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE} CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG=${CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG} python3 hack/kind_cluster.py setup

.PHONY: cluster.load-images
cluster.load-images:
	@$(CONTAINER_TOOL) exec ${KIND_CLUSTER_NAME}-control-plane crictl rmi ${CONTROLLER_MANAGER_CONTAINER_IMAGE} 2>/dev/null || true
	$(KIND) load docker-image ${CONTROLLER_MANAGER_CONTAINER_IMAGE} --name ${KIND_CLUSTER_NAME}

.PHONY: clean.cluster.kind
clean.cluster.kind:
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
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} ISTIO_GATEWAY_REVISION=${ISTIO_GATEWAY_REVISION} go test -tags=e2e ./test/e2e/... -v

.PHONY: test.tools
test.tools:
	cd tools/github_project_manager && go test -v ./...


# -------------------------------------------------------------------------------
# Coraza Coreruleset targets
# -------------------------------------------------------------------------------

CORERULESET_VERSION ?= v4.24.1
LOCALRULES ?= $(shell pwd)/tmp/rules
CORERULESET_DIR ?= $(shell pwd)/tmp/coreruleset
TMP_DOWNLOAD_DIR ?= $(shell pwd)/tmp/download
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
FTW_OVERRIDES ?= $(shell pwd)/test/conformance/.ftw-overrides.yml

# Verifies generator output for pinned CRS (CORERULESET_VERSION + --include-test-rule) against tools/corerulesetgen/testdata/coreruleset_parity.sha256.
.PHONY: coreruleset.verify-parity
coreruleset.verify-parity:
	@$(MAKE) CORERULESET_EXTRA_FLAGS="--include-test-rule" coraza.generaterules
	sha256sum -c tools/corerulesetgen/testdata/coreruleset_parity.sha256

.PHONY: test.conformance
test.conformance: coreruleset.verify-parity
	cd test/conformance &&  $(CONFORMANCE_EXTRA_FLAGS) FTW_CONFIG=$(shell pwd)/test/conformance/ftw.yml FTW_OVERRIDES=$(FTW_OVERRIDES) TESTMANIFESTS_PATH=$(CORERULESET_DIR)/tests/tests RULESET_PATH=$(LOCALRULES)/rules.yaml KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME} ISTIO_VERSION=${ISTIO_VERSION} ISTIO_GATEWAY_REVISION=${ISTIO_GATEWAY_REVISION} go test -tags=conformance ./... -v

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

HELM_CHART_DIR ?= charts/coraza-kubernetes-operator

.PHONY: helm.lint
helm.lint: ## Lint the Helm chart
	helm lint $(HELM_CHART_DIR)

.PHONY: helm.template
helm.template: ## Render the Helm chart templates locally
	helm template coraza-kubernetes-operator $(HELM_CHART_DIR) --namespace coraza-system

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
# Dependencies
# -------------------------------------------------------------------------------

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL ?= kubectl
KIND ?= kind
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
KUBE_API_LINTER = $(LOCALBIN)/golangci-lint-kube-api-linter

CONTROLLER_TOOLS_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION ?= v2.5.0
OPERATOR_SDK_VERSION ?= v1.42.0
KUBE_API_LINTER_VERSION ?= v0.0.0-20260206102632-39e3d06a2850

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
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
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
