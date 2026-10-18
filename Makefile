# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

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
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests against a kind cluster (see hack/e2e.sh).
	./hack/e2e.sh

.PHONY: demo
demo: ## Run M3's done-criterion on kind against Claude (needs ANTHROPIC_API_KEY; never run in CI; spends API credit).
	./hack/demo.sh

.PHONY: test-integration
test-integration: setup-envtest ## Run integration tests (requires Docker).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -tags integration ./... -run Integration -count=1 -v -timeout 20m

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
	go build -o bin/manager cmd/operator/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/operator/main.go

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
	- $(CONTAINER_TOOL) buildx create --name last-operator-builder
	$(CONTAINER_TOOL) buildx use last-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm last-operator-builder
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
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
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

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC ?= $(LOCALBIN)/protoc-gen-go-grpc
BUF_VERSION ?= v1.73.0
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2

.PHONY: buf
buf: $(BUF) ## Download buf locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))

.PHONY: protoc-gen-go
protoc-gen-go: $(PROTOC_GEN_GO) ## Download protoc-gen-go locally if necessary.
$(PROTOC_GEN_GO): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(PROTOC_GEN_GO_GRPC) ## Download protoc-gen-go-grpc locally if necessary.
$(PROTOC_GEN_GO_GRPC): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))

.PHONY: proto
proto: buf protoc-gen-go protoc-gen-go-grpc ## Lint the protobuf API and regenerate Go code.
	$(BUF) lint
	PATH="$(LOCALBIN):$$PATH" $(BUF) generate

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

.PHONY: envtest-assets
envtest-assets: ## Print the KUBEBUILDER_ASSETS path (downloads envtest binaries if needed).
	@$(MAKE) -s envtest >/dev/null
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

##@ Images and Helm

IMAGE_REGISTRY ?= ghcr.io/moogie-labs
IMAGE_TAG ?= dev
COMPONENTS ?= operator gateway audit-init

.PHONY: images
images: image-sandbox-base image-registry-gc ## Build container images for all components.
	@for c in $(COMPONENTS); do \
		$(CONTAINER_TOOL) build --build-arg CMD=$$c --build-arg VERSION=$(IMAGE_TAG) \
			-t $(IMAGE_REGISTRY)/last-$$c:$(IMAGE_TAG) . || exit 1; \
	done

.PHONY: image-sandbox-base
image-sandbox-base: ## Build the sandbox base image.
	$(CONTAINER_TOOL) build -f images/sandbox-base/Dockerfile -t $(IMAGE_REGISTRY)/last-sandbox-base:$(IMAGE_TAG) .

.PHONY: image-registry-gc
image-registry-gc: ## Build the registry garbage-collection sidecar image.
	$(CONTAINER_TOOL) build -f images/registry-gc/Dockerfile -t $(IMAGE_REGISTRY)/last-registry-gc:$(IMAGE_TAG) .

.PHONY: helm-crds
helm-crds: manifests ## Copy generated CRDs into the Helm chart.
	mkdir -p charts/last-operator/crds
	rm -f charts/last-operator/crds/*.yaml
	cp config/crd/bases/*.yaml charts/last-operator/crds/

.PHONY: nginx-check
nginx-check: ## Run nginx -t (in Docker) on the front proxy and pull proxy configs the chart renders.
	hack/nginx-check.sh

.PHONY: helm-lint
helm-lint: helm-crds ## Lint and render the Helm chart in bundled and external modes.
	helm lint --strict charts/last-operator
	helm template last charts/last-operator -n last-system >/dev/null
	helm template last charts/last-operator -n last-system \
		--set audit.bundled.enabled=false \
		--set audit.external.endpoint=https://s3.example.com \
		--set audit.external.existingSecret=audit-creds >/dev/null
	helm template last charts/last-operator -n last-system \
		--set audit.bundled.enabled=false \
		--set audit.external.endpoint=https://s3.example.com \
		--set audit.external.existingSecret=audit-creds \
		--set namespaces.create=false >/dev/null
	helm template last charts/last-operator -n last-system | grep -q 'name: last-sandbox-default-deny'
	helm template last charts/last-operator -n last-system | grep -q 'value: "system:serviceaccount:last-system:last-operator"'
	helm template last charts/last-operator -n last-system | grep -q 'containerPort: 9090'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \["tokenreviews"\]'
	helm template last charts/last-operator -n last-system | awk '/name: last-sandbox-runner/{f=1} f&&/^---/{exit} f' | grep -q '^automountServiceAccountToken: false'
	@# The sandbox egress gate uses the gateway's http port as a canary: the last-gateway NetworkPolicy
	@# ingress item that opens 8080 must exist and must have no from: key (before or after ports:).
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: NetworkPolicy\n/ && doc ~ /\nmetadata:\n  name: last-gateway\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | awk 'function close_item() { if (item ~ /port: 8080/) { found = 1; if (item ~ /(^|\n) *(- )?from:/) bad = 1 } item = "" } /^ *#/ { next } /^    - / { close_item() } /^[^ ]/ || /^  [a-z]/ { if (!/^  ingress:/) close_item() } { item = item $$0 "\n" } END { close_item(); exit !(found && !bad) }'
	helm template last charts/last-operator -n last-system | grep -q '^kind: ValidatingAdmissionPolicy$$'
	helm template last charts/last-operator -n last-system | grep -q 'name: LAST_EGRESS_CANARIES, value: "service/last-gateway/last-gateway:8080,node:10250"'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \[pods/log\]'
	helm template last charts/last-operator -n last-system | grep -q 'resources: \[serviceaccounts/token\]'
	helm template last charts/last-operator -n last-system | grep -q 'terminationGracePeriodSeconds: 330'
	helm template last charts/last-operator -n last-system | grep -q 'LAST_DRAIN_TIMEOUT, value: "300s"'
	helm template last charts/last-operator -n last-system | grep -q 'name: last-gateway-audit-guard'
	helm template last charts/last-operator -n last-system \
		--set audit.bundled.enabled=false --set audit.external.endpoint=https://s3.example.com \
		--set audit.external.existingSecret=audit-creds | grep -q 'last-gateway-audit-guard-external'
	helm template last charts/last-operator -n last-system \
		--set audit.bundled.enabled=false --set audit.external.endpoint=https://s3.example.com \
		--set audit.external.existingSecret=audit-creds | grep -q '"audit-creds"'
	! helm template last charts/last-operator -n last-system | grep -q 'last-gateway-audit-guard-external'
	helm template last charts/last-operator -n last-system --set sandbox.image=img:dev | grep -q 'name: LAST_SANDBOX_IMAGE, value: "img:dev"'
	! helm template last charts/last-operator -n last-system | grep -q 'LAST_SANDBOX_IMAGE'
	helm template last charts/last-operator -n last-system | grep -q 'image: ghcr.io/moogie-labs/last-operator:'
	helm template last charts/last-operator -n last-system | grep -q 'audience: last-gateway'
	helm template last charts/last-operator -n last-system | grep -q -- '--gateway-address=last-gateway.last-gateway.svc:9090'
	helm template last charts/last-operator -n last-system | grep -q 'name: last-operator-leader-election'
	@# The gateway's gRPC port admits only the operator's pods: the podSelector shares the namespaceSelector's from: element.
	helm template last charts/last-operator -n last-system | grep -A1 'kubernetes.io/metadata.name: last-system' | grep -q '^          podSelector:'
	@# The operator never gets Secrets or ManagedResource deletes (spec §4.1; test/admission checks the full list).
	! grep -Eq '^[[:space:]]+(resources|verbs): \[.*(secrets|delete)' charts/last-operator/files/operator-clusterrole.yaml
	@# The bundled registry: namespaces, credentials, and a front proxy that picks the realm and user file per method.
	helm template last charts/last-operator -n last-system | grep -q 'name: last-registry-front'
	helm template last charts/last-operator -n last-system | grep -q 'name: last-registry-auth'
	helm template last charts/last-operator -n last-system | grep -q 'pod-security.kubernetes.io/enforce: privileged'
	helm template last charts/last-operator -n last-system | grep -q 'map $$request_method $$realm'
	helm template last charts/last-operator -n last-system | grep -q 'DELETE /etc/nginx/auth/admin'
	@# helm re-salts the htpasswd lines on every render, so the front proxy must restart when last-registry-auth changes.
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: Deployment\n/ && doc ~ /\nmetadata:\n  name: last-registry-front\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | grep -q 'checksum/auth:'
	! helm template last charts/last-operator -n last-system --set registry.bundled.enabled=false | grep -q 'namespace: last-registry'
	@# Config lives in a subPath mount, so each proxy pod template must carry a config checksum to roll on a ConfigMap change.
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: Deployment\n/ && doc ~ /\nmetadata:\n  name: last-registry-front\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | grep -q 'checksum/config:'
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: DaemonSet\n/ && doc ~ /\nmetadata:\n  name: last-registry-pull-proxy\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | grep -q 'checksum/config:'
	@# The node-local pull proxy and the build namespace.
	helm template last charts/last-operator -n last-system | grep -q 'hostPort: 5000'
	helm template last charts/last-operator -n last-system | grep -q 'name: last-build-default-deny'
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: ConfigMap\n/ && doc ~ /\nmetadata:\n  name: last-registry-pull-proxy\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | grep -qF 'location ~ "^/v2/last/(images|base)/(manifests|blobs)/sha256:[0-9a-f]{64}\z" {'
	helm template last charts/last-operator -n last-system | awk 'function emit() { if (doc ~ /\nkind: ConfigMap\n/ && doc ~ /\nmetadata:\n  name: last-registry-pull-proxy\n/) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' | grep -q 'location / { return 403; }'
	@# The GC sidecar: shares the registry PVC read-write, sees only the admin htpasswd line (a directory mount, so kubelet refreshes it), and is reachable only from the gateway.
	@# Each check reads a render saved to a file, so a failed render fails the target instead of passing a negated grep.
	@r=$$(mktemp) && trap 'rm -f $$r' EXIT && helm template last charts/last-operator -n last-system >$$r && \
	doc() { awk -v k="$$1" -v n="$$2" 'function emit() { if (doc ~ ("\nkind: " k "\n") && doc ~ ("\nmetadata:\n  name: " n "\n")) printf "%s", doc; doc = "" } /^---/ { emit(); next } { doc = doc $$0 "\n" } END { emit() }' $$r; } && \
	dep=$$(doc Deployment last-registry) && test -n "$$dep" && \
	sc=$$(printf '%s\n' "$$dep" | awk '/^        - name: /{f=($$0=="        - name: registry-gc")} /^      [a-z]/{f=0} f') && \
	vol=$$(printf '%s\n' "$$dep" | awk '/^        - name: /{f=($$0=="        - name: gc-auth")} /^[^ ]/{f=0} f') && \
	test -n "$$sc" && test -n "$$vol" && \
	printf '%s\n' "$$sc" | grep -qE 'mountPath: /var/lib/registry\}$$' && \
	! printf '%s\n' "$$sc" | grep 'mountPath: /var/lib/registry' | grep -q readOnly && \
	printf '%s\n' "$$sc" | grep -E 'mountPath: /etc/registry-gc[,}]' | grep -q 'readOnly: true' && \
	! printf '%s\n' "$$sc" | grep -q subPath && \
	printf '%s\n' "$$sc" | grep -q 'allowPrivilegeEscalation: false' && \
	printf '%s\n' "$$sc" | grep -q 'drop: \[ALL\]' && \
	printf '%s\n' "$$sc" | grep -q '^          resources:' && \
	printf '%s\n' "$$sc" | grep -q 'name: gc, containerPort: 5001' && \
	printf '%s\n' "$$sc" | grep -A2 'readinessProbe:' | grep -q 'tcpSocket: {port: gc}' && \
	printf '%s\n' "$$sc" | grep -A2 'livenessProbe:' | grep -q 'tcpSocket: {port: gc}' && \
	test "$$(printf '%s\n' "$$vol" | grep -c 'key:')" = 1 && printf '%s\n' "$$vol" | grep -q 'key: admin, path: htpasswd' && \
	printf '%s\n' "$$vol" | grep -q 'secretName: last-registry-auth' && \
	printf '%s\n' "$$dep" | grep -q 'terminationGracePeriodSeconds: 1560' && \
	! printf '%s\n' "$$dep" | grep -q 'checksum/auth' && \
	doc Service last-registry-gc | grep -q 'port: 5001' && \
	doc NetworkPolicy last-registry | awk 'function close_item() { if (item ~ /port: 5001/) { found++; if (item ~ /port: 5000/ || item !~ /kubernetes.io\/metadata.name: last-gateway\n *podSelector:\n *matchLabels:\n *app.kubernetes.io\/name: last-gateway\n/ || item ~ /last-registry-front/) bad = 1 } item = "" } /^ *#/ { next } /^    - / { close_item() } /^[^ ]/ || /^  [a-z]/ { if (!/^  ingress:/) close_item() } { item = item $$0 "\n" } END { close_item(); exit !(found == 1 && !bad) }' && \
	grep -q 'name: LAST_IMAGE_GC_INTERVAL, value: "24h"' $$r && \
	grep -q 'name: LAST_REGISTRY_GC_URL, value: "http://last-registry-gc.last-registry.svc:5001"' $$r
	@r=$$(mktemp) && trap 'rm -f $$r' EXIT && helm template last charts/last-operator -n last-system --set registry.bundled.enabled=false >$$r && ! grep -q 'last-registry-gc' $$r
	helm template last charts/last-operator -n last-system | awk '/name: last-gateway-build/{f=1} f&&/^---/{exit} f' | grep -q 'resources: \[configmaps\]'
	helm template last charts/last-operator -n last-system | awk '/kind: Role$$/{r=1} r&&/name: last-gateway-build/{f=1} f&&/^---/{exit} f' | grep -A1 'resources: \[configmaps\]' | grep -q 'verbs: \[create, list, delete\]'
	@# Without the bundled registry there is no pull proxy, but the build namespace and the gateway's Role remain.
	! helm template last charts/last-operator -n last-system --set registry.bundled.enabled=false | grep -q 'hostPort'
	@# Image builds: the gateway knows the registry hosts and the BuildKit image, and can list the front Service's EndpointSlices.
	helm template last charts/last-operator -n last-system | grep -q 'name: LAST_BUILDKIT_IMAGE, value: "moby/buildkit:v0.26.2-rootless"'
	helm template last charts/last-operator -n last-system | grep -q 'name: LAST_REGISTRY_PULL_HOST, value: "localhost:5000"'
	helm template last charts/last-operator -n last-system | grep -q 'name: LAST_REGISTRY_FRONT_HOST, value: "last-registry-front.last-registry.svc:5000"'
	helm template last charts/last-operator -n last-system | awk '/^kind: Role$$/{r=1} r&&/name: last-gateway-registry/{f=1} f&&/^---/{exit} f' | grep -A1 'resources: \[endpointslices\]' | grep -q 'verbs: \[list\]'
	helm template last charts/last-operator -n last-system | awk '/^kind: Role$$/{r=1} r&&/name: last-gateway-registry/{f=1} f&&/^---/{exit} f' | grep -A1 'resources: \[services\]' | grep -q 'verbs: \[get\]'
	! helm template last charts/last-operator -n last-system --set registry.bundled.enabled=false | grep -q 'name: last-gateway-registry'
	helm template last charts/last-operator -n last-system --set registry.bundled.enabled=false | grep -q 'name: last-gateway-build'
