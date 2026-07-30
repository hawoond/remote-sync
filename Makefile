SHELL := /bin/sh

TOOLS_DIR := $(CURDIR)/.tools/bin
PROTOC_GEN_GO := $(TOOLS_DIR)/protoc-gen-go
PROTOC_GEN_GO_GRPC := $(TOOLS_DIR)/protoc-gen-go-grpc

.PHONY: tools generate format test vet build check clean

tools:
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	GOBIN=$(TOOLS_DIR) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2

generate: tools
	PATH="$(TOOLS_DIR):$$PATH" protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/sync/v1/sync.proto

format:
	gofmt -w $$(find . -name '*.go' -not -path './.tools/*')

test:
	go test -race ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -trimpath -o bin/sync-server ./cmd/sync-server
	go build -trimpath -o bin/sync-agent ./cmd/sync-agent

check: generate format test vet build

clean:
	rm -rf bin .tools coverage.out
