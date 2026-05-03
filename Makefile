# tfperms developer / CI entry-point.
#
# Constraints:
#   - Must work with macOS-shipped GNU Make 3.81. Avoid 4.x-only features
#     (.ONESHELL, --output-sync, &: grouped targets, $(file ...)). The
#     `tidy-check` recipe uses a single backslash-joined shell pipeline so
#     cleanup logic and the exit code share one shell process.
#   - `cp` (not `mv`) is used for the go.mod/go.sum snapshot/restore so the
#     inode is preserved — editors and file watchers see in-place edits.
#   - $(MAKEFILE_LIST) is used by `help` (not a hard-coded "Makefile") so
#     the auto-help mechanism still works if this file is ever included
#     from another Makefile.
#   - Tool-running targets (build/test/lint/release-snapshot) deliberately
#     do NOT use `@` prefixes. Surfacing the executed command in CI logs
#     is more valuable than a quieter terminal.

.DEFAULT_GOAL := help

.PHONY: help build test lint tidy-check catalog-validate release-snapshot

help: ## Show this help message
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the tfperms binary into the working directory
	go build -o tfperms ./cmd/tfperms

test: ## Run the full test suite
	go test ./...

lint: ## Run golangci-lint with .golangci.yml
	golangci-lint run

catalog-validate: ## Assert that the committed catalog satisfies all schema and provenance rules
	go test ./internal/catalog -run '^(TestCatalogValid|TestRepositoryCatalog)'

release-snapshot: ## Build a local snapshot release via goreleaser
	goreleaser release --snapshot --clean

tidy-check: ## Fail if `go mod tidy` would modify go.mod or go.sum
	@cp go.mod .go.mod.tidy-check.bak
	@cp go.sum .go.sum.tidy-check.bak
	@go mod tidy; \
	rc=0; \
	if ! cmp -s go.mod .go.mod.tidy-check.bak || ! cmp -s go.sum .go.sum.tidy-check.bak; then \
	  cp .go.mod.tidy-check.bak go.mod; \
	  cp .go.sum.tidy-check.bak go.sum; \
	  echo "go.mod or go.sum is not tidy. Run: go mod tidy" >&2; \
	  rc=1; \
	fi; \
	rm -f .go.mod.tidy-check.bak .go.sum.tidy-check.bak; \
	exit $$rc
