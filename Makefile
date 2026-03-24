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
ISTIO_VERSION ?= 1.28.2
METALLB_VERSION ?= 0.15.3
METALLB_POOL_SIZE ?= 128 # Defines the size of MetalLB pool, when being used

VERSION ?= v0.0.0-dev
IMAGE_REGISTRY ?= ghcr.io/networking-incubator
CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator
CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG ?= $(VERSION)
CONTROLLER_MANAGER_CONTAINER_IMAGE ?= ${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE}:${CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG}

# ------------------------------------------------------------------------------
# General
# ------------------------------------------------------------------------------

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: all
all: build

# ------------------------------------------------------------------------------
# Build
# ------------------------------------------------------------------------------

.PHONY: build
build: manifests generate fmt vet lint
	go build -o bin/manager -tags no_fs_access cmd/main.go

.PHONY: build.image
build.image:
	$(CONTAINER_TOOL) build -t ${CONTROLLER_MANAGER_CONTAINER_IMAGE} .

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
deploy: helm.sync ## Deploy operator into the cluster using Helm
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_DIR) \
		--namespace $(HELM_RELEASE_NAMESPACE) \
		--create-namespace \
		--set image.repository=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE) \
		--set image.tag=$(CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG)

.PHONY: undeploy
undeploy: ## Remove operator from the cluster using Helm
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_RELEASE_NAMESPACE)

.PHONY: run
run: manifests generate fmt vet
	go run ./cmd/main.go

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
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} go test -tags=integration ./test/integration/... -v

.PHONY: test.e2e
test.e2e:
	go clean -testcache
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) ISTIO_VERSION=${ISTIO_VERSION} go test -tags=e2e ./test/e2e/... -v

.PHONY: test.tools
test.tools:
	cd tools/cmd/github_project_manager && go test -v ./...


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
	@python3 hack/generate_coreruleset_configmaps.py --rules-dir $(CORERULESET_DIR)/rules/ --version $(CORERULESET_VERSION:v%=%) $(CORERULESET_EXTRA_FLAGS) > $(LOCALRULES)/rules.yaml

.PHONY: coraza.coreruleset
coraza.coreruleset: coraza.generaterules
	kubectl delete -n $(NAMESPACE) --ignore-not-found -f $(LOCALRULES)/*.yaml
	kubectl apply -n $(NAMESPACE) --server-side -f $(LOCALRULES)/*.yaml

# -------------------------------------------------------------------------------
# Coraza Coreruleset - Conformance test
# -------------------------------------------------------------------------------
CONFORMANCE_EXTRA_FLAGS ?= 

.PHONY: test.conformance
test.conformance:
	$(MAKE) CORERULESET_EXTRA_FLAGS="--include-test-rule" coraza.generaterules
	cd test/conformance &&  $(CONFORMANCE_EXTRA_FLAGS) FTW_CONFIG=$(shell pwd)/test/conformance/ftw.yml TESTMANIFESTS_PATH=$(CORERULESET_DIR)/tests/tests RULESET_PATH=$(LOCALRULES)/rules.yaml KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME} ISTIO_VERSION=${ISTIO_VERSION} go test -tags=conformance ./... -v

# -------------------------------------------------------------------------------
# OLM Bundle
# -------------------------------------------------------------------------------

BUNDLE_IMG_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator-bundle
BUNDLE_IMG_TAG ?= $(VERSION)
BUNDLE_IMG ?= $(BUNDLE_IMG_BASE):$(BUNDLE_IMG_TAG)

CATALOG_IMG_BASE ?= $(IMAGE_REGISTRY)/coraza-kubernetes-operator-catalog
CATALOG_IMG_TAG ?= $(VERSION)
CATALOG_IMG ?= $(CATALOG_IMG_BASE):$(CATALOG_IMG_TAG)
OPM_VERSION ?= v1.64.0
BUNDLE_DIR ?= bundle
CATALOG_DIR ?= catalog
CATALOG_FILE ?= $(CATALOG_DIR)/coraza-kubernetes-operator/catalog.yaml
OLM_CHANNEL ?= alpha

.PHONY: bundle
bundle: helm.sync ## Generate OLM bundle from Helm chart
	python3 hack/generate_bundle.py \
		--chart-dir $(HELM_CHART_DIR) \
		--bundle-dir $(BUNDLE_DIR) \
		--version $(VERSION) \
		--image $(CONTROLLER_MANAGER_CONTAINER_IMAGE) \
		--channels $(OLM_CHANNEL) \
		--default-channel $(OLM_CHANNEL)

.PHONY: bundle.build
bundle.build: ## Build the OLM bundle image
	$(CONTAINER_TOOL) build -f $(BUNDLE_DIR)/bundle.Dockerfile -t $(BUNDLE_IMG) $(BUNDLE_DIR)

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
helm.sync-rbac: manifests ## Sync generated RBAC rules into the Helm chart ClusterRole
	@GEN=config/rbac/role.yaml; \
	CHART=$(HELM_CHART_DIR)/templates/clusterrole.yaml; \
	sed '/^rules:/q' "$$CHART" > "$$CHART.tmp" && \
	sed '1,/^rules:/d' "$$GEN" >> "$$CHART.tmp" && \
	mv "$$CHART.tmp" "$$CHART"

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
