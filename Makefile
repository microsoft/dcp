.DEFAULT_GOAL := help

ifneq (3.81,$(firstword $(sort $(MAKE_VERSION) 3.81)))
    $(error This Makefile requires make version 3.81 or newer. You have make version $(MAKE_VERSION))
endif

# Detect the operating system, and configure shell and shell commands.
ifeq ($(OS),Windows_NT)
    detected_OS := Windows
	SHELL := pwsh.exe
	.SHELLFLAGS := -Command
	repo_dir := $(shell Get-Location | Select-Object -ExpandProperty Path)
	mkdir := New-Item -ItemType Directory -Force -Path
	rm_rf := Remove-Item -Recurse -Force -Path
	rm_f := Remove-Item -Force -Path
	home_dir := $(USERPROFILE)
	install := Copy-Item
	exe_suffix := .exe
else
    # -o pipefail will treat a pipeline as failed if one of the elements fail.
    SHELL := /usr/bin/env bash -o pipefail

    detected_OS := $(shell uname -s)
	repo_dir := $(shell pwd)
	mkdir := mkdir -p -m 0755
	rm_rf := rm -rf
	rm_f := rm -f
	home_dir := $(HOME)
	install := install -p
	exe_suffix :=
endif

# Honor GOOS settings from the environment to determine appropriate suffix.
# This will allow us to honor the naming scheme of the target OS rather than
# always matching the behavior of the current host OS.
ifeq ($(GOOS),windows)
	bin_exe_suffix := .exe
else ifeq ($(GOOS).$(OS),.Windows_NT)
	bin_exe_suffix := .exe
else
	bin_exe_suffix :=
endif

## Environment variables affecting build and installation
# Note these have to be defined before they are used in targets

# Locations and names for binaries built from this repository
OUTPUT_BIN ?= $(repo_dir)/bin
DCP_DIR ?= $(home_dir)/.dcp
EXTENSIONS_DIR ?= $(home_dir)/.dcp/ext
DCP_BINARY ?= ${OUTPUT_BIN}/dcp$(bin_exe_suffix)
DCPD_BINARY ?= ${OUTPUT_BIN}/ext/dcpd$(bin_exe_suffix)
DCPCTRL_BINARY ?= $(OUTPUT_BIN)/ext/dcpctrl$(bin_exe_suffix)
AZD_RENDERER_BINARY ?= $(OUTPUT_BIN)/ext/azdRenderer$(bin_exe_suffix)

# Locations and definitions for tool binaries
TOOL_BIN ?= $(repo_dir)/.toolbin
GOLANGCI_LINT ?= $(TOOL_BIN)/golangci-lint$(exe_suffix)
CONTROLLER_GEN ?= $(TOOL_BIN)/controller-gen$(exe_suffix)
OPENAPI_GEN ?= $(TOOL_BIN)/openapi-gen$(exe_suffix)
DELAY_TOOL ?= $(TOOL_BIN)/delay$(exe_suffix)
ENVTEST ?= $(TOOL_BIN)/setup-envtest$(exe_suffix)

# Tool Versions
GOLANGCI_LINT_VERSION ?= v1.53.3
CONTROLLER_TOOLS_VERSION ?= v0.12.0
CODE_GENERATOR_VERSION ?= v0.27.3
ENVTEST_K8S_VERSION = 1.27.1

# Disable C interop https://dave.cheney.net/2016/01/18/cgo-is-not-go
export CGO_ENABLED=0

ifeq ($(detected_OS),Windows)
    GO_SOURCES := $(shell Get-ChildItem -Include '*.go' -Recurse -File | Select-Object -ExpandProperty FullName | Select-String -NotMatch 'pkg\\generated\\')
else
    GO_SOURCES := $(shell find . -name '*.go' -type f -not -path "./pkg/generated/*")
endif

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
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


##@ Code generation

.PHONY: generate
generate: generate-object-methods generate-openapi generate-crd ## Generate object copy methods, OpenAPI definitions, and CRD definitions.

.PHONY: generate-object-methods
generate-object-methods: $(repo_dir)/api/v1/zz_generated.deepcopy.go ## Generates object copy methods for resourced defined in this repo
$(repo_dir)/api/v1/zz_generated.deepcopy.go : $(GO_SOURCES) controller-gen
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/v1/..."

define run-openapi-gen
$(OPENAPI_GEN) \
	--input-dirs github.com/microsoft/usvc-apiserver/api/v1 \
	--input-dirs "k8s.io/apimachinery/pkg/apis/meta/v1,k8s.io/apimachinery/pkg/runtime,k8s.io/apimachinery/pkg/version" \
	--output-package pkg/generated/openapi \
	-O zz_generated.openapi \
	--go-header-file $(repo_dir)/hack/boilerplate.go.txt \
	--output-base "$(repo_dir)" $(OPENAPI_GEN_OPTS)
endef

.PHONY: generate-openapi
generate-openapi: $(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go ## Generates OpenAPI definitions for resources defined in this repo

.PHONY: generate-openapi-debug
generate-openapi-debug: OPENAPI_GEN_OPTS = -v 6
generate-openapi-debug: $(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go ## Runs OpenAPI generator with additional options for debugging

$(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go: $(GO_SOURCES) openapi-gen
	$(run-openapi-gen)

# At run time DCP does not use CRDs, but they are used by tests.
crd_prefix = $(repo_dir)/pkg/generated/crd/usvc-dev.developer.microsoft.com_
crd_files = $(crd_prefix)containers.yaml $(crd_prefix)containervolumes.yaml $(crd_prefix)executables.yaml

.PHONY: generate-crd
generate-crd: $(crd_files) ## Generates CRD documents for resources defined in this repo
$(crd_files) : $(GO_SOURCES) controller-gen
	$(CONTROLLER_GEN) crd paths="./api/v1/..." output:crd:artifacts:config=pkg/generated/crd

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): | $(TOOL_BIN)
ifeq ($(detected_OS),Windows)
	if (-not (Test-Path "$(CONTROLLER_GEN)")) { $$env:GOBIN = "$(TOOL_BIN)"; go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); $$env:GOBIN = $$null; }
else
	[[ -s $(CONTROLLER_GEN) ]] || GOBIN=$(TOOL_BIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
endif

.PHONY: openapi-gen
openapi-gen: $(OPENAPI_GEN)
$(OPENAPI_GEN): | $(TOOL_BIN)
ifeq ($(detected_OS),Windows)
	if (-not (Test-Path "$(OPENAPI_GEN)")) { $$env:GOBIN = "$(TOOL_BIN)"; go install k8s.io/code-generator/cmd/openapi-gen@$(CODE_GENERATOR_VERSION); $$env:GOBIN = $$null; }
else
	[[ -s $(OPENAPI_GEN) ]] || GOBIN=$(TOOL_BIN) go install k8s.io/code-generator/cmd/openapi-gen@$(CODE_GENERATOR_VERSION)
endif

# delay-tool is used for process package testing
.PHONY: delay-tool
delay-tool: $(DELAY_TOOL)
$(DELAY_TOOL): $(wildcard ./test/delay/*.go) | $(TOOL_BIN)
	go build -o $(DELAY_TOOL) github.com/microsoft/usvc-apiserver/test/delay

##@ Development

release: BUILD_ARGS ?= -ldflags "-s -w"
release: compile ## Builds all binaries with flags to reduce binary size

build: generate compile ## Runs codegen and builds all binaries (DCP CLI, DCP API server, and controller host)
compile: build-dcpd build-dcpctrl build-dcp ## Builds all binaries (DCP CLI, DCP API server, and controller host) (skips codegen)

.PHONY: build-dcpd
build-dcpd: $(DCPD_BINARY) ## Builds DCP API server binary (dcpd)
$(DCPD_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN)
	go build -o $(DCPD_BINARY) $(BUILD_ARGS) ./cmd/dcpd

.PHONY: build-dcp
build-dcp: $(DCP_BINARY) ## Builds DCP CLI binary
$(DCP_BINARY): $(GO_SOURCES) go.mod | ${OUTPUT_BIN}
	go build -o $(DCP_BINARY) $(BUILD_ARGS) ./cmd/dcp

.PHONY: build-dcpctrl
build-dcpctrl: $(DCPCTRL_BINARY) ## Builds DCP standard controller host (dcpctrl)
$(DCPCTRL_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN)
	go build -o $(DCPCTRL_BINARY) $(BUILD_ARGS) ./cmd/dcpctrl

.PHONY: build-azdRenderer
build-azdRenderer: $(AZD_RENDERER_BINARY) ## Builds DCP Azure Developer CLI workload renderer (experimental)
$(AZD_RENDERER_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN)
	go build -o $(AZD_RENDERER_BINARY) $(BUILD_ARGS) ./cmd/azdRenderer

.PHONY: clean
clean: | ${OUTPUT_BIN} ${TOOL_BIN} ## Deletes build output (all binaries), and all cached tool binaries.
	$(rm_rf) $(OUTPUT_BIN)/*
	$(rm_rf) $(TOOL_BIN)/*

.PHONY: lint
lint: golangci-lint ## Runs the linter
# On Windows we use the global golangci-lint binary.
ifeq ($(detected_OS),Windows)
	golangci-lint run --timeout 5m
else
	$(GOLANGCI_LINT) run --timeout 5m
endif

.PHONY: install
install: compile | $(DCP_DIR) $(EXTENSIONS_DIR) ## Installs all binaries to their destinations
	$(install) $(DCPD_BINARY) $(EXTENSIONS_DIR)
	$(install) $(DCPCTRL_BINARY) $(EXTENSIONS_DIR)
	$(install) $(DCP_BINARY) $(DCP_DIR)

.PHONY: uninstall
uninstall: ## Uninstalls all binaries from their destinations
	$(rm_f) $(EXTENSIONS_DIR)/dcpd$(bin_exe_suffix)
	$(rm_f) $(EXTENSIONS_DIR)/dcpctrl$(bin_exe_suffix)
	$(rm_f) $(DCP_DIR)/dcp$(bin_exe_suffix)

ifneq ($(detected_OS),Windows)
.PHONY: link-dcp
link-dcp: ## Links the dcp binary to /usr/local/bin (macOS/Linux ONLY). Use 'sudo -E" to run this target (sudo -E make link-dcp). Typically it is a one-time operation (the symbolic link does not need to change when you modify the binary).
	ln -s -v $(DCP_DIR)/dcp$(bin_exe_suffix) /usr/local/bin/dcp$(bin_exe_suffix)
endif

##@ Test targets

ifeq ($(detected_OS),Windows)
define run-tests
$$env:KUBEBUILDER_ASSETS = "$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOL_BIN) -p path)"; go test ./... $(TEST_OPTS); $$env:KUBEBUILDER_ASSETS = $$null
endef
else
define run-tests
KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOL_BIN) -p path)" go test ./... $(TEST_OPTS)
endef
endif

define run-tests
go test ./... $(TEST_OPTS)
endef

.PHONY: test
test: TEST_OPTS = -coverprofile cover.out
test: delay-tool envtest ## Run all tests in the repository
	$(run-tests)

.PHONY: test-ci
ifeq ($(detected_OS),Windows)
# On Windows enabling -race requires additional components to be installed (gcc), so we do not support it at the moment.
test-ci: TEST_OPTS = -coverprofile cover.out -count 1
test-ci: lint delay-tool envtest
	$(run-tests)
else
test-ci: TEST_OPTS = -coverprofile cover.out -race -count 1
test-ci: lint delay-tool envtest ## Runs tests in a way appropriate for CI pipeline, with linting etc.
	CGO_ENABLED=1 $(run-tests)
endif


## Development and test support targets

${OUTPUT_BIN}:
	$(mkdir) ${OUTPUT_BIN}

$(TOOL_BIN):
	$(mkdir) $(TOOL_BIN)

$(EXTENSIONS_DIR):
	$(mkdir) $(EXTENSIONS_DIR)

$(DCP_DIR):
	$(mkdir) $(DCP_DIR)

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): | $(TOOL_BIN)
ifeq ($(detected_OS),Windows)
	if (-not (Test-Path "$(ENVTEST)")) { $$env:GOBIN = "$(TOOL_BIN)"; go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest; $$env:GOBIN = $$null; }
else
	[[ -s $(ENVTEST) ]] || GOBIN=$(TOOL_BIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
endif

.PHONY: golangci-lint
ifeq ($(detected_OS),Windows)
# golangci-lint does not have pwsh-compatible install script, so the user must install it manually
golangci-lint:
	@ try { golangci-lint --version } catch { throw "golangci-lint tool is missing. See https://golangci-lint.run/usage/install/#local-installation for installation instructions." }
else
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): | $(TOOL_BIN)
	[[ -s $(GOLANGCI_LINT) ]] || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(TOOL_BIN) $(GOLANGCI_LINT_VERSION)
endif
