# Image the docker-* and deploy targets use. Releases publish
# ghcr.io/efuturetoday/gojsop:<version>.
IMG ?= ghcr.io/efuturetoday/gojsop:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

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
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./api/...;./cmd/...;./internal/..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./api/...;./cmd/...;./internal/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -race $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= gojsop-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: demo-e2e
demo-e2e: ## Run the configmap-sync demo end-to-end on a kind cluster.
	./hack/e2e.sh

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Engine

include internal/jsengine/glue/versions.env

ENGINE_OS := $(shell uname -s | sed 's/Darwin/macos/;s/Linux/linux/')
ENGINE_ARCH := $(shell uname -m | sed 's/aarch64/arm64/')
WASI_SDK_DIR = $(LOCALBIN)/wasi-sdk-$(WASI_SDK_VERSION)
QUICKJS_DIR = $(LOCALBIN)/quickjs-ng-$(QUICKJS_VERSION)
BINARYEN_DIR = $(LOCALBIN)/binaryen-version_$(BINARYEN_VERSION)

.PHONY: engine-wasm
engine-wasm: ## Rebuild internal/jsengine/engine.wasm from glue.c with the pinned toolchain (internal/jsengine/glue/versions.env).
	@set -e; mkdir -p "$(LOCALBIN)"; \
	if [ ! -x "$(WASI_SDK_DIR)/bin/clang" ]; then \
	  echo "Downloading wasi-sdk $(WASI_SDK_VERSION)"; \
	  curl -fsSL "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-$(WASI_SDK_VERSION)/wasi-sdk-$(WASI_SDK_VERSION).0-$(ENGINE_ARCH)-$(ENGINE_OS).tar.gz" | tar -xz -C "$(LOCALBIN)"; \
	  mv "$(LOCALBIN)/wasi-sdk-$(WASI_SDK_VERSION).0-$(ENGINE_ARCH)-$(ENGINE_OS)" "$(WASI_SDK_DIR)"; \
	fi; \
	if [ ! -f "$(QUICKJS_DIR)/quickjs.c" ]; then \
	  echo "Downloading quickjs-ng v$(QUICKJS_VERSION)"; \
	  mkdir -p "$(QUICKJS_DIR)"; \
	  curl -fsSL "https://github.com/quickjs-ng/quickjs/archive/refs/tags/v$(QUICKJS_VERSION).tar.gz" | tar -xz -C "$(QUICKJS_DIR)" --strip-components=1; \
	fi; \
	if [ ! -x "$(BINARYEN_DIR)/bin/wasm-opt" ]; then \
	  echo "Downloading binaryen $(BINARYEN_VERSION)"; \
	  case "$(ENGINE_ARCH)" in arm64) bin_arch=$(if $(filter linux,$(ENGINE_OS)),aarch64,arm64);; *) bin_arch=$(ENGINE_ARCH);; esac; \
	  curl -fsSL "https://github.com/WebAssembly/binaryen/releases/download/version_$(BINARYEN_VERSION)/binaryen-version_$(BINARYEN_VERSION)-$$bin_arch-$(ENGINE_OS).tar.gz" | tar -xz -C "$(LOCALBIN)"; \
	fi; \
	WASI_SDK="$(WASI_SDK_DIR)" QUICKJS="$(QUICKJS_DIR)" WASM_OPT="$(BINARYEN_DIR)/bin/wasm-opt" internal/jsengine/glue/build.sh

.PHONY: sdlc-check
sdlc-check: ## Check .agents against the method of the SDLC library (github.com/efuturetoday/agentic-sdlc)
	GOPRIVATE=github.com/efuturetoday go run github.com/efuturetoday/agentic-sdlc/cmd/sdlc-check@v0.1.1

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: gojsop
gojsop: ## Build the gojsop CLI to bin/gojsop.
	go build -o bin/gojsop ./cmd/gojsop

CLI_TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: cli-dist
cli-dist: ## Build the gojsop CLI for every release target to dist/cli (VERSION=x.y.z).
	@test -n "$(VERSION)" || { echo "set VERSION"; exit 1; }
	@mkdir -p dist/cli; set -e; for target in $(CLI_TARGETS); do \
	  os=$${target%/*}; arch=$${target#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
	  echo "gojsop $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o "dist/cli/gojsop_$(VERSION)_$${os}_$${arch}$${ext}" ./cmd/gojsop; \
	done

.PHONY: sdk-test
sdk-test: gojsop ## Build and test the npm packages in sdk/ against bin/gojsop.
	cd sdk && npm ci && npm run build && cd testing && GOJSOP_BIN=$(CURDIR)/bin/gojsop npx vitest run

.PHONY: sdk-claim
sdk-claim: ## Once, after npm login: claim the npm package names and trust release.yml to publish them.
	cd sdk && node scripts/claim.ts

.PHONY: sdk-lint
sdk-lint: ## Check format and lint of the npm packages in sdk/ with Biome (sdk-format fixes).
	cd sdk && npm ci && npm run lint

.PHONY: sdk-format
sdk-format: ## Format the npm packages in sdk/ and apply safe Biome fixes.
	cd sdk && npm run format

.PHONY: sdk-pack
sdk-pack: ## Pack the npm packages as version 0.0.0-dev to sdk/build/tarballs, to install them into a local workspace.
	$(MAKE) cli-dist VERSION=0.0.0-dev
	cd sdk && npm ci && npm run build && node scripts/publish.ts --version 0.0.0-dev --bin-dir ../dist/cli --pack build/tarballs

.PHONY: sdk-smoke
sdk-smoke: ## Pack the npm packages, create a workspace from them and run its tests, as a user would.
	$(MAKE) cli-dist VERSION=0.0.0-smoke
	cd sdk && npm ci && npm run build && ./scripts/smoke.sh

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

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
	- $(CONTAINER_TOOL) buildx create --name gojsop-builder
	$(CONTAINER_TOOL) buildx use gojsop-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm gojsop-builder
	rm Dockerfile.cross

.PHONY: chart
chart: kubebuilder ## Generate deploy/chart/templates from config/ (Chart.yaml and values.yaml are kept by hand).
	@# kubebuilder edit runs make build-installer, which rewrites the image
	@# that release-please keeps in config/manager; keep that file as it is.
	@cp config/manager/kustomization.yaml "$(LOCALBIN)/kustomization.yaml.keep"
	@rm -rf deploy/chart/templates
	"$(KUBEBUILDER)" edit --plugins=helm/v2-alpha --output-dir=deploy; s=$$?; \
	mv "$(LOCALBIN)/kustomization.yaml.keep" config/manager/kustomization.yaml; exit $$s

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

CERT_MANAGER_VERSION ?= v1.16.1

.PHONY: install-certmanager
install-certmanager: ## Install cert-manager into the K8s cluster (idempotent). Required by the webhook stack.
	"$(KUBECTL)" apply -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
	"$(KUBECTL)" -n cert-manager wait --for=condition=Available --timeout=180s deploy/cert-manager deploy/cert-manager-webhook deploy/cert-manager-cainjector

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.20.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.7.2
KUBEBUILDER_VERSION ?= v4.16.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: kubebuilder
kubebuilder: $(KUBEBUILDER) ## Download kubebuilder locally if necessary.
$(KUBEBUILDER): $(LOCALBIN)
	$(call go-install-tool,$(KUBEBUILDER),sigs.k8s.io/kubebuilder/v4,$(KUBEBUILDER_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
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
