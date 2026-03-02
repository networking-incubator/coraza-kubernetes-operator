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

KIND_CLUSTER_NAME ?= coraza-kubernetes-operator-integration
ISTIO_VERSION ?= 1.28.2
METALLB_VERSION ?= 0.15.3
METALLB_POOL_SIZE ?= 128 # Defines the size of MetalLB pool, when being used

VERSION ?= dev
CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE ?= ghcr.io/networking-incubator/coraza-kubernetes-operator
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
	go build -o bin/manager cmd/main.go -tags no_fs_access

.PHONY: build.image
build.image:
	$(CONTAINER_TOOL) build -t ${CONTROLLER_MANAGER_CONTAINER_IMAGE} .

.PHONY: build.installer
build.installer: manifests generate kustomize
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${CONTROLLER_MANAGER_CONTAINER_IMAGE}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

.PHONY: release.manifests
release.manifests: manifests generate kustomize
	@echo "Building release manifest bundles..."
	@mkdir -p dist
	@echo "Building CRDs bundle..."
	"$(KUSTOMIZE)" build config/crd > dist/crds.yaml
	@echo "Building controller-manager bundle..."
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE}:${VERSION}
	"$(KUSTOMIZE)" build config/manager > dist/operator.yaml
	@echo "Building samples bundle..."
	cat config/samples/gateway.yaml > dist/samples.yaml
	echo "---" >> dist/samples.yaml
	cat config/samples/ruleset.yaml >> dist/samples.yaml
	echo "---" >> dist/samples.yaml
	cat config/samples/engine.yaml >> dist/samples.yaml
	@echo "Manifest bundles built successfully in dist/"
	@ls -lh dist/

# ------------------------------------------------------------------------------
# Deployment
# ------------------------------------------------------------------------------

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${CONTROLLER_MANAGER_CONTAINER_IMAGE}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

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

# ------------------------------------------------------------------------------
# Test Cluster
# ------------------------------------------------------------------------------

.PHONY: cluster.kind
cluster.kind:
	ISTIO_VERSION=${ISTIO_VERSION} METALLB_VERSION=${METALLB_VERSION} METALLB_POOL_SIZE=${METALLB_POOL_SIZE} python3 hack/kind_cluster.py setup

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
	KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME} ISTIO_VERSION=${ISTIO_VERSION} go test -tags=integration ./test/integration/... -v

.PHONY: test.tools
test.tools:
	cd tools/cmd/github_issue_manager && go test -v ./...


# -------------------------------------------------------------------------------
# Coraza Coreruleset targets
# -------------------------------------------------------------------------------

CORERULESET_VERSION ?= v4.23.0
LOCALRULES ?= $(shell pwd)/tmp/rules
CORERULESET_DIR ?= $(shell pwd)/tmp/coreruleset
TMP_DOWNLOAD_DIR ?= $(shell pwd)/tmp/download
NAMESPACE ?= default
CORERULESET_EXTRA_FLAGS ?= --ignore-pmFromFile

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
	@python3 hack/generate_coreruleset_configmaps.py --rules-dir $(CORERULESET_DIR)/rules/ $(CORERULESET_EXTRA_FLAGS) > $(LOCALRULES)/rules.yaml

.PHONY: coraza.coreruleset
coraza.coreruleset: coraza.generaterules
	kubectl delete -n $(NAMESPACE) --ignore-not-found -f $(LOCALRULES)/*.yaml
	kubectl apply -n $(NAMESPACE) --server-side -f $(LOCALRULES)/*.yaml

# TODO: Deploy a Gateway, set port-forward and log-forward and run ftw passing the right flags
# -------------------------------------------------------------------------------
# Coraza Coreruleset - FTW testing
# -------------------------------------------------------------------------------

FTW_NAMESPACE ?= ftw-test
# TODO: we should get this from the created manifests
GATEWAY_NAME ?= coraza-gateway 
FTW_OUTPUT_FORMAT ?= plain
FTW_EXTRA_ARGS ?= 

.PHONY: ftw.environment
ftw.environment: cluster.kind
	kubectl delete ns --ignore-not-found $(FTW_NAMESPACE)
	kubectl create ns $(FTW_NAMESPACE)
	kubectl apply -n $(FTW_NAMESPACE) -f config/samples/
	kubectl wait deploy -n $(FTW_NAMESPACE) -l gateway.networking.k8s.io/gateway-name=$(GATEWAY_NAME) --for=condition=Available

.PHONY: ftw.coreruleset
ftw.coreruleset:
	$(MAKE) CORERULESET_EXTRA_FLAGS="--include-test-rule --ignore-pmFromFile" NAMESPACE=$(FTW_NAMESPACE) coraza.coreruleset

.PHONY: ftw.run
ftw.run: coraza.coreruleset.download
	# Give some time for rules to be properly loaded by the Gateway
	sleep 10
	$(KIND) get kubeconfig --name $(KIND_CLUSTER_NAME) > $(shell pwd)/tmp/kubeconfig
	python ftw/run.py --namespace $(FTW_NAMESPACE) --gateway $(GATEWAY_NAME) --config-file $(shell pwd)/ftw/ftw.yml --rules-directory $(CORERULESET_DIR)/tests/tests --kubeconfig $(shell pwd)/tmp/kubeconfig --output-format $(FTW_OUTPUT_FORMAT) $(FTW_EXTRA_ARGS)

.PHONY: ftw
ftw: ftw.environment ftw.coreruleset ftw.run

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
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION ?= v2.5.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

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
