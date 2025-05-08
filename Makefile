.DEFAULT_GOAL := help

ifneq (3.81,$(firstword $(sort $(MAKE_VERSION) 3.81)))
	$(error This Makefile requires make version 3.81 or newer. You have make version $(MAKE_VERSION))
endif

# Detect the operating system, and configure shell and shell commands.
ifeq ($(OS),Windows_NT)
	detected_OS := windows
	detected_arch := amd64
	SHELL := pwsh.exe
	.SHELLFLAGS := -Command
	repo_dir := $(shell Get-Location | Select-Object -ExpandProperty Path)
	mkdir := New-Item -ItemType Directory -Force -Path
	copy := Copy-Item -Force -Path
	rm_rf := Remove-Item -Recurse -Force -Path
	rm_f := Remove-Item -Force -Path
	home_dir := $(USERPROFILE)
	install := Copy-Item
	exe_suffix := .exe
	BUILD_TIMESTAMP ?= $(shell Get-Date -UFormat %FT%TZ -AsUTC)
	CLEAR_GOARGS := $$env:GOOS=""; $$env:GOARCH="";
else
	# -o pipefail will treat a pipeline as failed if one of the elements fail.
	SHELL := /usr/bin/env bash -o pipefail

	detected_OS := $(shell uname -s | awk '{print tolower($$0)}')
	detected_arch := $(shell uname -m)
	repo_dir := $(shell pwd)
	mkdir := mkdir -p -m 0755
	copy := cp -f
	rm_rf := rm -rf
	rm_f := rm -f
	home_dir := $(HOME)
	install := install -p
	exe_suffix :=
	BUILD_TIMESTAMP ?= $(shell date -u +%FT%TZ)
	CLEAR_GOARGS := GOOS="" GOARCH=""
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

ifeq ($(GOOS).$(detected_OS),.$(detected_OS))
	build_os := $(detected_OS)
else
	build_os := $(GOOS)
endif

ifeq ($(GOARCH).$(detected_arch),.x86_64)
	build_arch := amd64
	detected_arch := amd64
else ifeq ($(GOARCH).$(detected_arch),.$(detected_arch))
	build_arch := $(detected_arch)
else
	build_arch := $(GOARCH)
endif

ifeq ($(build_arch),amd64)
	GOVERSIONINFO_ARCH_FLAGS := -64
else ifeq ($(build_arch),arm64)
	GOVERSIONINFO_ARCH_FLAGS := -arm -64
else ifeq ($(build_arch),arm)
	GOVERSIONINFO_ARCH_FLAGS := -arm
else
	GOVERSIONINFO_ARCH_FLAGS :=
endif

## Environment variables affecting build and installation
# Note these have to be defined before they are used in targets

# Locations and names for binaries built from this repository
OUTPUT_BIN ?= $(repo_dir)/bin
DCP_DIR ?= $(home_dir)/.dcp
EXTENSIONS_DIR ?= $(home_dir)/.dcp/ext
BIN_DIR ?= $(home_dir)/.dcp/ext/bin
DCP_BINARY ?= ${OUTPUT_BIN}/dcp$(bin_exe_suffix)
DCPCTRL_BINARY ?= $(OUTPUT_BIN)/ext/dcpctrl$(bin_exe_suffix)
DCPPROC_BINARY ?= $(OUTPUT_BIN)/ext/bin/dcpproc$(bin_exe_suffix)

# Locations and definitions for tool binaries
GO_BIN ?= go
TOOL_BIN ?= $(repo_dir)/.toolbin
GOLANGCI_LINT ?= $(TOOL_BIN)/golangci-lint$(exe_suffix)
GOTOOL_BIN ?= $(GO_BIN) tool
CONTROLLER_GEN ?= $(GOTOOL_BIN) sigs.k8s.io/controller-tools/cmd/controller-gen
OPENAPI_GEN ?= $(GOTOOL_BIN) k8s.io/kube-openapi/cmd/openapi-gen
GOVERSIONINFO_GEN ?= $(GOTOOL_BIN) github.com/josephspurrier/goversioninfo/cmd/goversioninfo
DELAY_TOOL ?= $(TOOL_BIN)/delay$(exe_suffix)
LFWRITER_TOOL ?= $(TOOL_BIN)/lfwriter$(exe_suffix)
GO_LICENSES ?= $(GOTOOL_BIN) github.com/google/go-licenses/v2

# Tool Versions
GOLANGCI_LINT_VERSION ?= v1.64.7

# DCP Version information
VERSION ?= dev
VERSION_MAJOR ?= 0
VERSION_MINOR ?= 0
VERSION_PATCH ?= 0
COMMIT ?= $(shell git rev-parse HEAD)

version_values := -X 'github.com/microsoft/usvc-apiserver/internal/version.ProductVersion=$(VERSION)' -X 'github.com/microsoft/usvc-apiserver/internal/version.CommitHash=$(COMMIT)' -X 'github.com/microsoft/usvc-apiserver/internal/version.BuildTimestamp=$(BUILD_TIMESTAMP)'

# CGO_ENABLED has to be enabled (set to 1) for FIPS compliant builds
export CGO_ENABLED ?= 0

ifeq ($(detected_OS),windows)
	GO_SOURCES := $(shell Get-ChildItem -Include '*.go' -Exclude 'zz_generated*' -Recurse -File | Select-Object -ExpandProperty FullName)
	TYPE_SOURCES := $(shell Get-ChildItem -Path './api/v1/*' -Include '*.go' -Exclude 'zz_generated*' -File | Select-Object -ExpandProperty FullName)
else
	GO_SOURCES := $(shell find . -name '*.go' -not -name 'zz_generated*' -type f)
	TYPE_SOURCES := $(shell find ./api/v1 -name '*.go' -not -name 'zz_generated*' -type f)
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
generate: generate-object-methods generate-openapi generate-goversioninfo ## Generate object copy methods, OpenAPI definitions, and binary version info.

.PHONY: generate-ci
generate-ci: generate generate-licenses ## Generate all codegen artifacts and licenses/notice files.

.PHONY: generate-object-methods
generate-object-methods: $(repo_dir)/api/v1/zz_generated.deepcopy.go ## Generates object copy methods for resourced defined in this repo
$(repo_dir)/api/v1/zz_generated.deepcopy.go : $(TYPE_SOURCES)
	$(CLEAR_GOARGS) $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/v1/..."

define run-openapi-gen
$(CLEAR_GOARGS) $(OPENAPI_GEN) \
	--output-pkg pkg/generated/openapi \
	--output-file pkg/generated/openapi/zz_generated.openapi.go \
	--go-header-file $(repo_dir)/hack/boilerplate.go.txt \
	--output-dir "$(repo_dir)" \
	--report-filename - \
	$(OPENAPI_GEN_OPTS) \
	github.com/microsoft/usvc-apiserver/api/v1 \
	k8s.io/apimachinery/pkg/apis/meta/v1 k8s.io/apimachinery/pkg/runtime k8s.io/apimachinery/pkg/version
endef

.PHONY: generate-openapi
generate-openapi: $(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go ## Generates OpenAPI definitions for resources defined in this repo

.PHONY: generate-openapi-debug
generate-openapi-debug: OPENAPI_GEN_OPTS = -v 6
generate-openapi-debug: $(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go ## Runs OpenAPI generator with additional options for debugging

$(repo_dir)/pkg/generated/openapi/zz_generated.openapi.go: $(TYPE_SOURCES)
	$(run-openapi-gen)

.PHONY: generate-goversioninfo
generate-goversioninfo:
ifeq ($(build_os),windows)
	$(CLEAR_GOARGS) $(GOVERSIONINFO_GEN) $(GOVERSIONINFO_ARCH_FLAGS) -o $(repo_dir)/cmd/dcp/resource.syso -product-version "$(VERSION) $(COMMIT)" -ver-major=$(VERSION_MAJOR) -ver-minor=$(VERSION_MINOR) -ver-patch=$(VERSION_PATCH) -ver-build=0 $(repo_dir)/cmd/dcp/versioninfo.json ## Generates version information for Windows binaries
	$(copy) $(repo_dir)/cmd/dcp/resource.syso $(repo_dir)/cmd/dcpctrl/resource.syso
	$(copy) $(repo_dir)/cmd/dcp/resource.syso $(repo_dir)/cmd/dcpproc/resource.syso
else
	-$(rm_f) $(repo_dir)/cmd/dcp/resource.syso
	-$(rm_f) $(repo_dir)/cmd/dcpctrl/resource.syso
	-$(rm_f) $(repo_dir)/cmd/dcpproc/resource.syso
endif

.PHONY: generate-licenses
generate-licenses: generate-dependency-notices ## Generates license/notice files for all dependencies

# # We ignore the standard library (go list std) as a workaround for https://github.com/google/go-licenses/issues/244.
# The awk script converts the output of `go list std` (line separated modules) to the input that `--ignore` expects
.PHONY: generate-dependency-notices
generate-dependency-notices:
	$(CLEAR_GOARGS) $(GO_LICENSES) report ./cmd/dcp ./cmd/dcpctrl ./cmd/dcpproc --template NOTICE.tmpl --ignore github.com/microsoft/usvc-apiserver --ignore $(shell go list std | awk 'NR > 1 { printf(",") } { printf("%s",$$0) } END { print "" }') > NOTICE

# delay-tool is used for process package testing
.PHONY: delay-tool
delay-tool: $(DELAY_TOOL)
$(DELAY_TOOL): $(wildcard ./test/delay/*.go) | $(TOOL_BIN)
	$(GO_BIN) build -o $(DELAY_TOOL) github.com/microsoft/usvc-apiserver/test/delay

# lfwriter tool is used for testing lockfile package
.PHONY: lfwriter-tool
lfwriter-tool: $(LFWRITER_TOOL)
$(LFWRITER_TOOL): $(wildcard ./test/lfwriter/*.go) | $(TOOL_BIN)
	$(GO_BIN) build -o $(LFWRITER_TOOL) github.com/microsoft/usvc-apiserver/test/lfwriter

##@ Development

# Note: Go runtime is incompatible with C/C++ stack protection feature https://github.com/golang/go/blob/master/src/runtime/cgo/cgo.go#L28 More info/rationale https://github.com/golang/go/issues/21871#issuecomment-329330371
release: BUILD_ARGS := $(BUILD_ARGS) -buildmode=pie -ldflags "-bindnow -s -w $(version_values)"
release: build-dcpproc build-dcpctrl build-dcp ## Builds all binaries with flags to reduce binary size

compile: BUILD_ARGS := $(BUILD_ARGS) -ldflags "$(version_values)"
compile: build-dcpproc build-dcpctrl build-dcp ## Builds DCP CLI and controller host (skips codegen)

compile-debug: BUILD_ARGS := $(BUILD_ARGS) -gcflags="all=-N -l" -ldflags "$(version_values)"
compile-debug: build-dcpproc build-dcpctrl build-dcp ## Builds DCP CLI and controller host with debug symbols (good for debugging; skips codegen)

build: generate compile ## Runs codegen and builds DCP CLI and controller host

build-ci: generate-ci release ## Runs codegen, including license/notice files, then builds DCP CLI and controller host with flags to reduce binary size

.PHONY: build-dcp
build-dcp: $(DCP_BINARY) ## Builds DCP CLI binary
$(DCP_BINARY): $(GO_SOURCES) go.mod | ${OUTPUT_BIN}
	$(GO_BIN) build -o $(DCP_BINARY) $(BUILD_ARGS) ./cmd/dcp

.PHONY: build-dcpctrl
build-dcpctrl: $(DCPCTRL_BINARY) ## Builds DCP standard controller host (dcpctrl)
$(DCPCTRL_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN)
	$(GO_BIN) build -o $(DCPCTRL_BINARY) $(BUILD_ARGS) ./cmd/dcpctrl

.PHONY: build-dcpproc
build-dcpproc: $(DCPPROC_BINARY) ## Builds DCP process monitor (dcpproc)
$(DCPPROC_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN)
	$(GO_BIN) build -o $(DCPPROC_BINARY) $(BUILD_ARGS) ./cmd/dcpproc

.PHONY: clean
clean: | ${OUTPUT_BIN} ${TOOL_BIN} ## Deletes build output (all binaries), and all cached tool binaries.
	$(rm_rf) $(OUTPUT_BIN)/*
	$(rm_rf) $(TOOL_BIN)/*

.PHONY: lint
lint: golangci-lint ## Runs the linter
# On Windows we use the global golangci-lint binary.
ifeq ($(detected_OS),windows)
	golangci-lint run --timeout 10m
else
	$(GOLANGCI_LINT) run --timeout 10m
endif

.PHONY: install
install: compile | $(DCP_DIR) $(EXTENSIONS_DIR) $(BIN_DIR) ## Installs all binaries to their destinations
	$(install) $(DCPPROC_BINARY) $(BIN_DIR)
	$(install) $(DCPCTRL_BINARY) $(EXTENSIONS_DIR)
	$(install) $(DCP_BINARY) $(DCP_DIR)

.PHONY: uninstall
uninstall: ## Uninstalls all binaries from their destinations
	$(rm_f) $(BIN_DIR)/dcpproc$(bin_exe_suffix)
	$(rm_f) $(EXTENSIONS_DIR)/dcpctrl$(bin_exe_suffix)
	$(rm_f) $(DCP_DIR)/dcp$(bin_exe_suffix)

ifneq ($(detected_OS),windows)
.PHONY: link-dcp
link-dcp: ## Links the dcp binary to /usr/local/bin (macOS/Linux ONLY). Use 'sudo -E" to run this target (sudo -E make link-dcp). Typically it is a one-time operation (the symbolic link does not need to change when you modify the binary).
	ln -s -v $(DCP_DIR)/dcp$(bin_exe_suffix) /usr/local/bin/dcp$(bin_exe_suffix)
endif

##@ Test targets

test-prereqs: BUILD_ARGS := $(BUILD_ARGS) -gcflags="all=-N -l" -ldflags "$(version_values)"
test-prereqs: build-dcp build-dcpproc delay-tool lfwriter-tool ## Ensures all prerequisites for running tests are built (run this before running tests selectively)

.PHONY: test-ci-prereqs
test-ci-prereqs: build-dcp build-dcpproc delay-tool lfwriter-tool

.PHONY: test
ifeq ($(CGO_ENABLED),0)
test: TEST_OPTS = -coverprofile cover.out -count 1 -parallel 32
test: test-prereqs ## Run all tests in the repository
	$(GO_BIN) test ./... $(TEST_OPTS)
else
test: TEST_OPTS = -coverprofile cover.out -race -count 1
test: test-prereqs ## Run all tests in the repository
	$(GO_BIN) test ./... $(TEST_OPTS)
endif

.PHONY: test-ci
ifeq ($(CGO_ENABLED),0)
# On Windows enabling -race requires additional components to be installed (gcc), so we do not support it at the moment.
test-ci: TEST_OPTS = -coverprofile cover.out -count 1
test-ci: test-ci-prereqs ## Runs tests in a way appropriate for CI pipeline, with linting etc.
	$(GO_BIN) test ./... $(TEST_OPTS)
else
test-ci: TEST_OPTS = -coverprofile cover.out -race -count 1
test-ci: test-ci-prereqs ## Runs tests in a way appropriate for CI pipeline, with linting etc.
	$(GO_BIN) test ./... $(TEST_OPTS)
endif

.PHONY: test-extended
test-extended: test-prereqs ## Run all tests, including tests that require special environment setup
ifeq ($(detected_OS),windows)
	$$env:DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES = "true"; $(GO_BIN) test ./... -count 1; $$env:DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES = $$null;
else ifeq ($(CGO_ENABLED),1)
	DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES="true" $(GO_BIN) test ./... -count 1 -race
else
	DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES="true" $(GO_BIN) test ./... -count 1
endif

## Development and test support targets

${OUTPUT_BIN}:
	$(mkdir) ${OUTPUT_BIN}

${OUTPUT_BIN}/ext/bin/: | ${OUTPUT_BIN}
	$(mkdir) ${OUTPUT_BIN}/ext/
	$(mkdir) ${OUTPUT_BIN}/ext/bin/

$(TOOL_BIN):
	$(mkdir) $(TOOL_BIN)

$(EXTENSIONS_DIR):
	$(mkdir) $(EXTENSIONS_DIR)

$(BIN_DIR):
	$(mkdir) $(BIN_DIR)

$(DCP_DIR):
	$(mkdir) $(DCP_DIR)

.PHONY: golangci-lint
ifeq ($(detected_OS),windows)
# golangci-lint does not have pwsh-compatible install script, so the user must install it manually
golangci-lint:
	@ try { golangci-lint --version } catch { throw "golangci-lint tool is missing. See https://golangci-lint.run/usage/install/#local-installation for installation instructions." }
else
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): | $(TOOL_BIN)
	[[ -s $(GOLANGCI_LINT) ]] || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(TOOL_BIN) $(GOLANGCI_LINT_VERSION)
endif
