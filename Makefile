IMG ?= controller:latest
YEAR ?= $(shell date +%Y)

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: check-generate
check-generate: manifests generate ## Verify committed generated output matches a fresh run.
	@dirty="$$(git status --porcelain)"; \
	if [ -n "$$dirty" ]; then \
		echo "Generated output is stale. Run 'make manifests generate' and commit the result." >&2; \
		echo "$$dirty" >&2; \
		git --no-pager diff >&2; \
		exit 1; \
	fi

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run unit tests. No cluster or envtest binaries needed.
	go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-crd
test-crd: manifests generate setup-envtest ## Run CRD validation and Event tests against a real API server.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags envtest ./internal/controller/ -run 'TestCRD|TestEnvtest' -count=1

KIND_CLUSTER ?= b2-backup-operator-test-e2e
KIND_NODE_IMAGE ?= kindest/node:v1.37.0
override E2E_KUBECONFIG = $(LOCALBIN)/e2e-$(KIND_CLUSTER).kubeconfig

.PHONY: setup-test-e2e
setup-test-e2e: kind $(LOCALBIN) ## Set up a Kind cluster for e2e tests if it does not exist
	@if "$(KIND)" get clusters 2>/dev/null | grep -qxF '$(KIND_CLUSTER)'; then \
		echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation."; \
	else \
		echo "Creating Kind cluster '$(KIND_CLUSTER)' from $(KIND_NODE_IMAGE)..."; \
		"$(KIND)" create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --kubeconfig "$(E2E_KUBECONFIG)"; \
	fi; \
	"$(KIND)" get kubeconfig --name $(KIND_CLUSTER) > "$(E2E_KUBECONFIG).tmp"; \
	mv -f "$(E2E_KUBECONFIG).tmp" "$(E2E_KUBECONFIG)"

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests against the Kind cluster only, through its own kubeconfig.
	trap '$(MAKE) cleanup-test-e2e' EXIT; \
	KUBECONFIG="$(E2E_KUBECONFIG)" KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -count=1 -ginkgo.v

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Tear down the Kind cluster used for e2e tests, if it exists
	@if "$(KIND)" get clusters 2>/dev/null | grep -qxF '$(KIND_CLUSTER)'; then \
		"$(KIND)" delete cluster --name $(KIND_CLUSTER) --kubeconfig "$(E2E_KUBECONFIG)"; \
	else \
		echo "Kind cluster '$(KIND_CLUSTER)' does not exist. Nothing to clean up."; \
	fi; \
	rm -f "$(E2E_KUBECONFIG)" "$(E2E_KUBECONFIG).tmp"

.PHONY: demo-env
demo-env: kind $(LOCALBIN) ## Create the b2bk-demo Kind cluster with the operator, a B2 stub, an S3 server and seeded backups.
	KIND="$(KIND)" KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) hack/demo/demo.sh up

.PHONY: demo
demo: demo-env vhs ttyd ## Record docs/demo/*.gif against the b2bk-demo Kind cluster.
	KIND="$(KIND)" hack/demo/demo.sh stage
	cd docs/demo && for tape in tui cli; do \
		KIND="$(KIND)" PATH="$(LOCALBIN):$$PATH" "$(VHS)" $$tape.tape || exit 1; \
	done

.PHONY: demo-clean
demo-clean: kind ## Delete the b2bk-demo Kind cluster and its images.
	KIND="$(KIND)" hack/demo/demo.sh clean

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	trap '$(CONTAINER_TOOL) buildx rm b2-backup-operator-builder >/dev/null 2>&1 || true; rm -f Dockerfile.cross' EXIT; \
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross; \
	$(CONTAINER_TOOL) buildx create --name b2-backup-operator-builder || true; \
	$(CONTAINER_TOOL) buildx use b2-backup-operator-builder; \
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .

define render-config
	tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	cp -R config "$$tmp/config" && \
	(cd "$$tmp/config/manager" && "$(KUSTOMIZE)" edit set image controller=$(IMG)) && \
	"$(KUSTOMIZE)" build "$$tmp/config/default"
endef

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	$(render-config) > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$("$(KUSTOMIZE)" build config/crd)"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$("$(KUSTOMIZE)" build config/crd)"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(render-config) | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
VHS ?= $(LOCALBIN)/vhs
TTYD ?= $(LOCALBIN)/ttyd

KUSTOMIZE_VERSION ?= v5.8.3
KIND_VERSION ?= v0.33.0
CONTROLLER_TOOLS_VERSION ?= v0.22.0

ENVTEST_VERSION ?= $(call required,$(call gomodver,sigs.k8s.io/controller-runtime),Set ENVTEST_VERSION manually (controller-runtime replace has no tag))

ENVTEST_K8S_VERSION ?= $(shell printf '%s' '$(call required,$(call gomodver,k8s.io/api),Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag))' | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
VHS_VERSION ?= v0.12.1
TTYD_VERSION ?= 1.7.7
TTYD_SHA256_x86_64 = 8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55
TTYD_SHA256_aarch64 = b38acadd89d1d396a0f5649aa52c539edbad07f4bc7348b27b4f4b7219dd4165
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

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

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml || exit 0; \
	if ! "$(GOLANGCI_LINT)" custom --help >/dev/null 2>&1; then exit 0; fi; \
	if "$(GOLANGCI_LINT)" version 2>/dev/null | grep -q custom; then exit 0; fi; \
	echo "Building custom golangci-lint with plugins..."; \
	"$(GOLANGCI_LINT)" custom --destination "$(LOCALBIN)" --name golangci-lint-custom && \
	mv -f "$(LOCALBIN)/golangci-lint-custom" "$(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION)"

.PHONY: vhs
vhs: $(VHS) ## Download vhs locally if necessary.
$(VHS): $(LOCALBIN)
	$(call go-install-tool,$(VHS),github.com/charmbracelet/vhs,$(VHS_VERSION))

.PHONY: ttyd
ttyd: $(TTYD) ## Download ttyd, which vhs runs, locally if necessary.
$(TTYD): $(LOCALBIN)
	@[ -f "$(TTYD)-$(TTYD_VERSION)" ] || { \
		set -e; \
		arch=$$(uname -m); \
		case $$arch in x86_64) sum=$(TTYD_SHA256_x86_64) ;; aarch64|arm64) arch=aarch64; sum=$(TTYD_SHA256_aarch64) ;; \
			*) echo "No pinned ttyd build for $$arch; install ttyd $(TTYD_VERSION) on PATH" >&2; exit 1 ;; esac; \
		curl -fsSL -o "$(TTYD)-$(TTYD_VERSION).tmp" https://github.com/tsl0922/ttyd/releases/download/$(TTYD_VERSION)/ttyd.$$arch; \
		echo "$$sum  $(TTYD)-$(TTYD_VERSION).tmp" | sha256sum -c --quiet; \
		chmod +x "$(TTYD)-$(TTYD_VERSION).tmp"; \
		mv "$(TTYD)-$(TTYD_VERSION).tmp" "$(TTYD)-$(TTYD_VERSION)"; \
	}; \
	ln -sf "$$(realpath "$(TTYD)-$(TTYD_VERSION)")" "$(TTYD)"

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

required = $(if $(strip $(1)),$(strip $(1)),$(error $(2)))
