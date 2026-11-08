.PHONY: build test race cover lint fmt tidy fuzz check help

# Version printed by `cel2yaml --version`, without the leading "v" of the
# tag; empty outside a checkout, the binary then prints dev.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -X main.version=$(VERSION)

# golangci-lint at the version tools/go.mod pins: a module of its own, its
# dependencies stay out of the library's go.mod.
GOLANGCI_LINT := go tool -modfile=tools/go.mod golangci-lint

# The Go files, but those of dot directories (.claude/worktrees...).
GOFILES = $(shell find . -name '*.go' -not -path './.*')

build: ## Compile the CLI to bin/cel2yaml
	go build -ldflags "$(LDFLAGS)" -o bin/cel2yaml ./cmd/cel2yaml

test: ## Run all tests
	go test ./...

race: ## Run all tests with the race detector
	go test -race ./...

cover: ## Run all tests with coverage: cover.out and the total
	go test -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -n 1

lint: ## Run golangci-lint, the version tools/go.mod pins
	$(GOLANGCI_LINT) run ./...

fmt: ## Format the Go files (gofmt -w), listing those changed
	@gofmt -l -w $(GOFILES)

tidy: ## Tidy go.mod and tools/go.mod
	go mod tidy
	go -C tools mod tidy

# The fuzz tests, <package>:<test>: go test fuzzes one at a time.
FUZZ := .:FuzzScan .:FuzzParse .:FuzzHostCost
FUZZTIME ?= 30s

fuzz: ## Run each fuzz test for FUZZTIME (30s)
	@for f in $(FUZZ); do \
		echo "go test ./$${f%%:*} -run '^$$' -fuzz '^$${f#*:}$$' -fuzztime $(FUZZTIME)"; \
		go test "./$${f%%:*}" -run '^$$' -fuzz "^$${f#*:}\$$" -fuzztime $(FUZZTIME) || exit 1; \
	done

check: ## Run what CI runs: gofmt, vet, lint, tidy, build and race tests
	@out=$$(gofmt -l $(GOFILES)); test -z "$$out" || { echo "not formatted, run make fmt:" >&2; echo "$$out" >&2; exit 1; }
	go vet ./...
	$(GOLANGCI_LINT) run ./...
	go mod tidy -diff
	go -C tools mod tidy -diff
	go build ./...
	go test -race ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
