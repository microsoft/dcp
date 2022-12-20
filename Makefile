
# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

build: build-dcpd build-dcp

LOCALBIN ?= $(shell pwd)/bin
${LOCALBIN}:
	mkdir -p ${LOCALBIN}

GOLANGCI_LINT := golangci-lint
export CGO_ENABLED=0

.PHONY: test
test:
	go test ./...

.PHONY: run-dcpd
run-dcpd:
	go run ./cmd/dcpd/main.go --secure-port=9562 --token=outdoor-salad --kubeconfig ./kubeconfig

.PHONY: build-dcpd
build-dcpd: ${LOCALBIN}
	go build -o ${LOCALBIN}/dcpd ./cmd/dcpd

.PHONY: build-dcp
build-dcp: ${LOCALBIN}
	go build -o ${LOCALBIN}/dcp ./cmd/dcp

.PHONY: clean
clean:
	rm -f ${LOCALBIN}/dcpd
	rm -f ${LOCALBIN}/dcp

.PHONY: lint
lint:
	${GOLANGCI_LINT} run
