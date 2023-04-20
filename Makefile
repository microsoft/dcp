.DEFAULT_GOAL := help

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail

ifneq (3.81,$(firstword $(sort $(MAKE_VERSION) 3.81)))
$(error This Makefile requires make version 3.81 or newer. You have make version $(MAKE_VERSION))
endif

## Environment variables affecting build and installation
# Note these have to be defined before they are used in targets

# Locations and names for binaries built from this repository
OUTPUT_BIN ?= $(shell pwd)/bin
DCP_DIR ?= $(HOME)/.dcp
EXTENSIONS_DIR ?= $(HOME)/.dcp/ext
LOCAL_BIN_DIR ?= /usr/local/bin
DCPD_BINARY ?= ${OUTPUT_BIN}/dcpd
DCP_BINARY ?= ${OUTPUT_BIN}/dcp

# Locations and definitions for tool binaries
TOOL_BIN ?= $(shell pwd)/.toolbin
GOLANGCI_LINT ?= $(TOOL_BIN)/golangci-lint
INSTALL ?= install -p
MKDIR ?= mkdir -p -m 0755

# Tool Versions
GOLANGCI_LINT_VERSION ?= v1.51.2

# Disable C interop https://dave.cheney.net/2016/01/18/cgo-is-not-go
export CGO_ENABLED=0

GO_SOURCES := $(shell find . -name '*.go' -type f -not -path "./external/*")


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


##@ Development

build: build-dcpd build-dcp ## Builds all binaries (DCP CLI and DCP API server)

.PHONY: build-dcpd
build-dcpd: $(DCPD_BINARY) ## Builds DCP API server binary (dcpd)
$(DCPD_BINARY): $(GO_SOURCES) go.mod | $(OUTPUT_BIN) 
	go build -o $(DCPD_BINARY) ./cmd/dcpd

.PHONY: build-dcp
build-dcp: $(DCP_BINARY) ## Builds DCP CLI binary
$(DCP_BINARY): $(GO_SOURCES) go.mod | ${OUTPUT_BIN}
	go build -o $(DCP_BINARY) ./cmd/dcp

.PHONY: clean
clean: ## Deletes build output (all binaries), and all cached tool binaries.
	rm -rf $(OUTPUT_BIN)/*
	rm -rf $(TOOL_BIN)/*

.PHONY: lint
lint: golangci-lint ## Runs the linter
	${GOLANGCI_LINT} run --timeout 5m

.PHONY: install
install: build | $(DCP_DIR) $(EXTENSIONS_DIR) ## Installs all binaries to their destinations
	$(INSTALL) $(DCPD_BINARY) $(EXTENSIONS_DIR)
	$(INSTALL) $(DCP_BINARY) $(DCP_DIR)

.PHONY: uninstall
uninstall: ## Uninstalls all binaries from their destinations
	rm -f $(EXTENSIONS_DIR)/dcpd
	rm -f $(DCP_DIR)/dcp

.PHONY: link-dcp
link-dcp: ## Links the dcp binary to /usr/local/bin. Use 'sudo -E" to run this target (sudo -E make link-dcp). Typically it is a one-time operation (the symbolic link does not need to change when you modify the binary).
	ln -s -v $(DCP_DIR)/dcp $(LOCAL_BIN_DIR)/dcp

##@ Testing
.PHONY: test
test: ## Run all tests in the repository
	go test ./...


## Development and test support targets

${OUTPUT_BIN}:
	$(MKDIR) ${OUTPUT_BIN}

$(TOOL_BIN):
	$(MKDIR) $(TOOL_BIN)

$(EXTENSIONS_DIR):
	$(MKDIR) $(EXTENSIONS_DIR)

$(DCP_DIR):
	$(MKDIR) $(DCP_DIR)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): | $(TOOL_BIN)
	[[ -s $(TOOL_BIN)/golangci-lint ]] || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(TOOL_BIN) $(GOLANGCI_LINT_VERSION)
