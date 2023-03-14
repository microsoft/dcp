
# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail

ifneq (3.81,$(firstword $(sort $(MAKE_VERSION) 3.81)))
$(error This Makefile requires make version 3.81 or newer. You have make version $(MAKE_VERSION))
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


##@ Development

build: build-dcpd build-dcp ## Builds all binaries (DCP CLI and DCP API server)

.PHONY: build-dcpd
build-dcpd: ${OUTPUT_BIN} ## Builds DCP API server binary (dcpd)
	go build -o ${OUTPUT_BIN}/dcpd ./cmd/dcpd

.PHONY: build-dcp
build-dcp: ${OUTPUT_BIN} ## Builds DCP CLI binary
	go build -o ${OUTPUT_BIN}/dcp ./cmd/dcp

.PHONY: test
test: ## Run all tests in the repository
	go test ./...

.PHONY: run-dcpd
run-dcpd: ## Runs DCP API server (dcpd) from the sources
	go run ./cmd/dcpd/main.go --secure-port=9562 --token=outdoor-salad --kubeconfig ./kubeconfig

.PHONY: clean
clean: ## Deletes build output (all binaries), and all cached tool binaries.
	rm -rf ${OUTPUT_BIN}/*
	rm -rf ${TOOL_BIN}/*

.PHONY: lint
lint: golangci-lint ## Runs the linter
	${GOLANGCI_LINT} run


##@ Build dependencies and environment variables

## Location for binaries built from this repo
OUTPUT_BIN ?= $(shell pwd)/bin
${OUTPUT_BIN}:
	mkdir -p ${OUTPUT_BIN}

# Location for tool binaries
TOOL_BIN ?= $(shell pwd)/.toolbin
$(TOOL_BIN):
	mkdir -p $(TOOL_BIN)

# Disable C interop https://dave.cheney.net/2016/01/18/cgo-is-not-go
export CGO_ENABLED=0

## Tool binaries
GOLANGCI_LINT ?= $(TOOL_BIN)/golangci-lint

## Tool Versions
GOLANGCI_LINT_VERSION ?= v1.51.2

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary
$(GOLANGCI_LINT): $(TOOL_BIN)
	[[ -s $(TOOL_BIN)/golangci-lint ]] || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(TOOL_BIN) $(GOLANGCI_LINT_VERSION)
