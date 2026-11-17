# rql-go — developer and CI entry points for the conformance release.
#
# Recipes run under /bin/sh (make's default) and avoid bash-isms, so this works on any
# POSIX shell. Every target is declared .PHONY. Run `make help` for the annotated list.
#
# Tool versions are pinned in tools-versions.txt; install them with `make tools`.
# The gate is `make check` (fmt-check + vet + lint + test -race + sec), which is what CI
# enforces as well.

SHELL := /bin/sh
.DEFAULT_GOAL := help

GO                ?= go
AWK               ?= awk
BENCH_COUNT       ?= 10
FUZZTIME          ?= 30s
COVER_PROFILE     ?= /tmp/rql-go-coverage.out

GOLANGCI_LINT     ?= golangci-lint
GOSEC             ?= gosec
GOVULNCHECK       ?= govulncheck

GOLANGCI_LINT_VERSION := $(shell $(AWK) '$$1=="golangci-lint"{print $$2}' tools-versions.txt)
GOSEC_VERSION         := $(shell $(AWK) '$$1=="gosec"{print $$2}' tools-versions.txt)
GOVULNCHECK_VERSION   := $(shell $(AWK) '$$1=="govulncheck"{print $$2}' tools-versions.txt)
BENCHSTAT_VERSION     := $(shell $(AWK) '$$1=="benchstat"{print $$2}' tools-versions.txt)
ACTIONLINT_VERSION    := $(shell $(AWK) '$$1=="actionlint"{print $$2}' tools-versions.txt)

.PHONY: help all build test race cover vet fmt fmt-check lint gosec govulncheck sec \
        bench bench-before bench-after bench-baseline bench-compare benchstat fuzz \
        fuzz-translate fuzz-path budget tidy-check tools docs clean check

help: ## Show this help
	@printf 'rql-go — available targets:\n\n'
	@$(AWK) 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@printf '\nVariables: BENCH_COUNT=%s FUZZTIME=%s COVER_PROFILE=%s\n' \
		'$(BENCH_COUNT)' '$(FUZZTIME)' '$(COVER_PROFILE)'
	@printf 'Tools:     run "make tools" to install the versions in tools-versions.txt\n'

all: check ## Alias for check

build: ## Compile every package
	$(GO) build ./...

test: ## Run unit tests (no race detector)
	$(GO) test -count=1 ./...

race: ## Run unit tests with the race detector, shuffled order
	$(GO) test -race -count=1 -shuffle=on ./...

cover: ## Run tests and print the total coverage
	$(GO) test -count=1 -covermode=atomic -coverprofile=$(COVER_PROFILE) ./...
	@$(GO) tool cover -func=$(COVER_PROFILE) | tail -n 1

vet: ## Run go vet (library and the nested integration module)
	$(GO) vet ./...
	cd test/integration && $(GO) vet ./...

fmt-check: ## Verify gofmt/goimports formatting (via golangci-lint fmt)
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo 'fmt-check: $(GOLANGCI_LINT) not found; run: make tools'; exit 1; }
	$(GOLANGCI_LINT) fmt --diff

fmt: ## Apply gofmt/goimports fixes
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo 'fmt: $(GOLANGCI_LINT) not found; run: make tools'; exit 1; }
	$(GOLANGCI_LINT) fmt ./...

lint: ## Run golangci-lint with the repo configuration
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo 'lint: $(GOLANGCI_LINT) not found; run: make tools'; exit 1; }
	$(GOLANGCI_LINT) run ./...
	cd test/integration && $(GOLANGCI_LINT) run ./...

gosec: ## Run gosec static analysis
	@command -v $(GOSEC) >/dev/null 2>&1 || { echo 'gosec: $(GOSEC) not found; run: make tools'; exit 1; }
	$(GOSEC) -quiet -fmt=text ./...

govulncheck: ## Run govulncheck (reported only for code paths this module calls)
	@command -v $(GOVULNCHECK) >/dev/null 2>&1 || { echo 'govulncheck: $(GOVULNCHECK) not found; run: make tools'; exit 1; }
	$(GOVULNCHECK) ./...

sec: gosec govulncheck ## Run the security gate (gosec + govulncheck)

bench: bench-after ## Run benchmarks into /tmp/bench-after.txt (BENCH_COUNT=10)

bench-before: ## Capture a benchmark baseline into /tmp/bench-before.txt
	./scripts/bench.sh before

bench-after: ## Run benchmarks into /tmp/bench-after.txt
	./scripts/bench.sh after

bench-baseline: ## Run benchmarks and record them as the new /tmp/bench-before.txt baseline
	./scripts/bench.sh baseline

bench-compare: ## Print benchstat diff: /tmp/bench-before.txt vs /tmp/bench-after.txt
	./scripts/bench.sh compare

benchstat: bench-compare ## Alias for bench-compare

fuzz: fuzz-translate fuzz-path ## Run every fuzz target for FUZZTIME (default 30s)

fuzz-translate: ## Fuzz the translator entry point for FUZZTIME (default 30s)
	$(GO) test -run '^$$' -fuzz '^FuzzTranslate$$' -fuzztime=$(FUZZTIME) ./fuzz

fuzz-path: ## Fuzz nested path/field parsing with injection payloads
	$(GO) test -run '^$$' -fuzz '^FuzzPathSegment$$' -fuzztime=$(FUZZTIME) ./fuzz

budget: ## Assert the allocation budget (docs/DESIGN.md §6)
	./scripts/check_alloc.sh

tidy-check: ## Fail if go.mod/go.sum are not tidy (does not write files)
	$(GO) mod tidy -diff

tools: ## Install pinned tools from tools-versions.txt
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(GO) install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION)
	$(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	@printf 'tools: installed golangci-lint %s, gosec %s, govulncheck %s, benchstat %s, actionlint %s\n' \
		'$(GOLANGCI_LINT_VERSION)' '$(GOSEC_VERSION)' '$(GOVULNCHECK_VERSION)' '$(BENCHSTAT_VERSION)' '$(ACTIONLINT_VERSION)'

docs: ## Validate the design/performance docs and render the package API
	@test -f docs/DESIGN.md || { echo 'docs: docs/DESIGN.md is missing'; exit 1; }
	@test -f docs/PERFORMANCE.md || { echo 'docs: docs/PERFORMANCE.md is missing'; exit 1; }
	@printf 'docs: DESIGN.md and PERFORMANCE.md present\n'
	$(GO) doc -all .

clean: ## Remove local build artifacts (coverage profile, test cache)
	rm -f '$(COVER_PROFILE)'
	$(GO) clean -testcache

integration: ## Run the Postgres integration suite (needs RQL_TEST_POSTGRES_DSN)
	@if [ -z "$$RQL_TEST_POSTGRES_DSN" ]; then \
		echo "integration: set RQL_TEST_POSTGRES_DSN, e.g."; \
		echo "  RQL_TEST_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/db?sslmode=disable'"; \
		exit 1; \
	fi
	cd test/integration && $(GO) test -count=1 -race ./...

check: fmt-check vet lint race sec ## Full local gate: fmt-check + vet + lint + test -race + sec
