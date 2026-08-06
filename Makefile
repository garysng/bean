GOBIN := $(shell go env GOPATH)/bin
PROTO_FILES := $(shell find proto -name '*.proto')

# Pinned so generated code is reproducible across machines and CI.
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

.PHONY: all build proto proto-tools test test-e2e lint vet cover clean

proto-tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

all: build

build:
	go build ./...

proto:
	PATH="$(GOBIN):$$PATH" protoc \
		--proto_path=proto \
		--go_out=internal/gen --go_opt=module=github.com/garysng/bean/internal/gen \
		--go-grpc_out=internal/gen --go-grpc_opt=module=github.com/garysng/bean/internal/gen \
		$(PROTO_FILES)

test:
	go test -race -count=1 ./...

cover:
	go test -race -count=1 -coverprofile=coverage.out \
		-coverpkg=./internal/beand/...,./internal/node/...,./internal/control/...,./cli/... \
		./internal/... ./cli/...
	go tool cover -func=coverage.out | tail -1

test-e2e:
	go test -race -count=1 -tags=e2e ./tests/e2e/...

vet:
	go vet ./...

lint: vet
	test -z "$$(gofmt -l . | grep -v internal/gen)" || (gofmt -l . | grep -v internal/gen; exit 1)
	hack/check-ascii.sh

# preflight is exactly what CI's go job runs, in the same order.
#
# It exists because a hand-assembled sweep is not the same check. A PR failed on
# gofmt after build, vet and the ascii check had all passed locally -- the one
# step not replicated was the one that failed, and the whole point of checking
# before pushing is defeated by a list that is almost the CI list.
#
# --commits matches CI, which checks unpushed commit messages too, so a Chinese
# character in a commit message is caught while the rewrite is still free.
preflight: build
	test -z "$$(gofmt -l . | grep -v internal/gen)" || (gofmt -l . | grep -v internal/gen; exit 1)
	go vet ./...
	hack/check-ascii.sh --commits
	$(MAKE) cover

clean:
	rm -f coverage.out
	go clean ./...
