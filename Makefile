# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	$(MAKE) helm-crds

HELM_CHART ?= charts/trino-maintenance-operator

.PHONY: helm-crds
helm-crds: ## Copy the generated CRDs into the Helm chart (keeps chart CRDs in sync with config/crd/bases).
	cp config/crd/bases/*.yaml $(HELM_CHART)/crds/

.PHONY: _helm-lint
_helm-lint:
	helm lint $(HELM_CHART) --set gateway.url=http://trino-gateway:8080
	helm template test $(HELM_CHART) --namespace trino-operator-system \
		--set gateway.url=https://trino-gateway.example.com \
		--set gateway.credentialsSecret.name=trino-gateway-auth \
		--set metrics.enabled=true --set podDisruptionBudget.enabled=true --set replicaCount=2 > /dev/null
	helm lint integrations/argocd/trino-chart
	@test "$$(helm template test integrations/argocd/trino-chart | grep -c '^kind: TrinoMaintenance' || :)" = 0
	@test "$$(helm template test integrations/argocd/trino-chart --set maintenance.hooksEnabled=true | grep -c '^kind: TrinoMaintenance')" = 2

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# The host only starts Compose. Testcontainers uses a separate, short-lived
# Docker daemon; even its privileged K3s node cannot see a host kubeconfig.
TEST_DOCKER_SOCKET ?= /var/run/docker.sock
TEST_DOCKER = docker --host "unix://$(TEST_DOCKER_SOCKET)"
TEST_COMPOSE = BUILDX_BUILDER=default $(TEST_DOCKER) compose --env-file /dev/null -f "$(CURDIR)/test/compose.yaml" -p "$$project"
include test/images.env
.PHONY: test helm-lint test-controller test-e2e e2e-argocd e2e-trino
test: TEST_SERVICE = unit
helm-lint: TEST_SERVICE = charts
test-controller: export TEST_NAME = TestControllers
test-controller: export TEST_PACKAGE = ./internal/controller
test-controller: export POD_IMAGES = $(PAUSE_IMAGE) $(DNS_IMAGE)
e2e-argocd: export POD_IMAGES = $(PAUSE_IMAGE) $(DNS_IMAGE) $(ARGO_IMAGE) $(REDIS_IMAGE) $(CHART_SERVER_IMAGE)
test-controller e2e-argocd: PUBLIC_IMAGES = $(RYUK_IMAGE) $(K3S_IMAGE) $(POD_IMAGES)
e2e-trino: PUBLIC_IMAGES = $(RYUK_IMAGE) $(TRINO_IMAGE) $(GATEWAY_IMAGE) $(POSTGRES_IMAGE)
e2e-argocd: export TEST_NAME = TestArgoUpgrade
e2e-argocd: export TEST_TARGET = argo
e2e-trino: export TEST_NAME = TestRealTrinoDrain
e2e-argocd e2e-trino: export TEST_PACKAGE = ./test/e2e
test: ## Fast Go tests in a network-isolated container, no Kubernetes or nested Docker.
helm-lint: ## Lint and render both charts offline in a container.
test-controller: ## Controller regressions against a fresh Testcontainers K3s.
e2e-argocd: ## Real Argo CD sync/failure/repair with three lightweight backends in K3s.
e2e-trino: ## Real Gateway, Postgres and two single-node Trinos, no Kubernetes.
test helm-lint test-controller e2e-argocd e2e-trino:
	@project=tmo-test-$$$$-$$RANDOM; cfg=$$(mktemp -d); export DOCKER_CONFIG="$$cfg"; unset DOCKER_CONTEXT; \
	trap '$(TEST_COMPOSE) down --volumes --rmi local; rm -rf -- "$$cfg"' EXIT; \
	if [ -n "$(PUBLIC_IMAGES)" ]; then \
	  $(TEST_COMPOSE) up --wait engine; \
	  for img in $(PUBLIC_IMAGES); do $(TEST_DOCKER) image inspect "$$img" >/dev/null 2>&1 || $(TEST_DOCKER) pull "$$img"; done; \
	  $(TEST_DOCKER) save $(PUBLIC_IMAGES) | $(TEST_COMPOSE) exec -T engine docker load; \
	fi; \
	$(TEST_COMPOSE) up --build --abort-on-container-exit --exit-code-from $(or $(TEST_SERVICE),integration) $(or $(TEST_SERVICE),integration)
test-e2e: e2e-argocd

.PHONY: lint
lint: golangci-lint ## Run golangci-lint and Kubernetes structured logging checks.
	"$(GOLANGCI_LINT)" run
	go run sigs.k8s.io/logtools/logcheck@v0.11.0 ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name trino-maintenance-operator-builder
	$(CONTAINER_TOOL) buildx use trino-maintenance-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm trino-maintenance-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef
