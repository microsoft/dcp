
OUT_DIR := ./bin
GOLANGCI_LINT := golangci-lint
export CGO_ENABLED=0

.PHONY: test
test:
	go test ./...

.PHONY: run-dcpd
run-dcpd:
	go run ./cmd/dcpd/main.go --secure-port=9562 --token=outdoor-salad

.PHONY: build-dcpd
build-dcpd:
	go build -o ${OUT_DIR}/dcpd ./cmd/dcpd

.PHONY: clean
clean:
	rm ${OUT_DIR}/dcpd
