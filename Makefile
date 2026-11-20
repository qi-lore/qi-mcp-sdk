# Developer entry points for qi-mcp-sdk.
#
# Every gate here mirrors something the repo already promises: .github/workflows/ci.yml
# (build, vet, lint, race tests), the artifact-isolation rule in AGENTS.md / FR-033, and
# the zero-`require` rule for the root module (SC-002/SC-003/SC-009).
#
# GNU Make 3.81 is what macOS ships, so this file deliberately avoids 4.x-only
# features. Recipes are one shell command per line; do not rely on `make -j` for
# ordering — `check` and `ci` re-invoke make serially on purpose.
#
# Quick start:
#   make              # help
#   make check        # everything CI runs, plus formatting and artifact scope
#   make test PKG=./core RUN=TestDecode

SHELL := /bin/sh
.DEFAULT_GOAL := help

GO      ?= go
GOLANGCI ?= golangci-lint
MODULE  := $(shell awk '/^module /{print $$2; exit}' go.mod)
GO_LANG := $(shell awk '/^go /{print $$2; exit}' go.mod)
PKG     ?= ./...
RUN     ?=
TASKS   := specs/001-go-mcp-sdk/tasks.md

COVER_FILE ?= coverage.out
COVER_HTML ?= coverage.html

.PHONY: help versions build vet lint fmt fmt-check test test-fast cover cover-html \
        check ci artifact-scope deps deps-root tasks clean clean-cache tidy

help: ## Show this help
	@printf 'Usage: make <target> [PKG=./pkg] [RUN=TestName]\n\n'
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_.-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf '\nArguments: PKG=./pkg and RUN=TestName apply to test, test-fast, cover, cover-html.\n'

versions: ## Print toolchain and repo state
	@printf 'module         %s\n' '$(MODULE)'
	@printf 'go directive   %s\n' '$(GO_LANG)'
	@printf 'go             %s\n' "$$($(GO) version)"
	@printf 'golangci-lint  %s\n' "$$($(GOLANGCI) version 2>&1 | head -1)"
	@printf 'branch         %s\n' "$$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo '(no git)')"

build: ## Compile every package (go build ./...)
	$(GO) build ./...

vet: ## Run go vet (go vet ./...)
	$(GO) vet ./...

lint: ## Run golangci-lint v2; refuses to run v1, which silently misses violations
	@out=$$($(GOLANGCI) version 2>&1) || { \
		echo 'golangci-lint not found on PATH — install the v2 release (see AGENTS.md)'; exit 1; }; \
	case "$$out" in \
		*"version 2."*) ;; \
		*) echo "golangci-lint v2 is required, got: $$out"; \
		   echo 'v1.64.8 reports false passes for real code; see AGENTS.md "Local toolchain paths"'; \
		   exit 1 ;; \
	esac
	$(GOLANGCI) run ./...

fmt: ## Apply gofmt + goimports via the configured linter
	$(GOLANGCI) fmt

fmt-check: ## Fail if any file is not formatted
	@$(GOLANGCI) fmt --diff || { \
		echo; echo 'formatting drift above — run `make fmt`'; exit 1; }

test: ## Race-enabled tests (PKG=..., RUN=...)
	$(GO) test -race $(if $(strip $(RUN)),-run '$(RUN)') $(PKG)

test-fast: ## Tests without -race, for quick iteration
	$(GO) test $(if $(strip $(RUN)),-run '$(RUN)') $(PKG)

cover: ## Write COVER_FILE and print the total
	$(GO) test -race -covermode=atomic -coverprofile=$(COVER_FILE) $(if $(strip $(RUN)),-run '$(RUN)') $(PKG)
	@$(GO) tool cover -func=$(COVER_FILE) | tail -1

cover-html: ## Re-run cover, then render COVER_HTML
	@$(MAKE) --no-print-directory cover
	$(GO) tool cover -html=$(COVER_FILE) -o $(COVER_HTML)
	@echo "wrote $(COVER_HTML)"

check: ## Full local gate: formatting, build, vet, lint, race tests, artifacts, root deps
	@$(MAKE) --no-print-directory fmt-check
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory vet
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory test
	@$(MAKE) --no-print-directory artifact-scope
	@$(MAKE) --no-print-directory deps
	@echo 'check: all gates passed'

ci: ## Exactly what .github/workflows/ci.yml runs (build, vet, lint, race tests)
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory vet
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory test
	@echo 'ci parity: passed'

artifact-scope: ## Assert specs/, .specify/, .opencode/, docs/ are ignored and untracked
	@set -e; \
	status=0; \
	for entry in 'specs/' '.specify/' '.opencode/' 'docs/'; do \
		if ! grep -qxF "$$entry" .gitignore; then \
			echo "FAIL: $$entry is not listed in .gitignore (FR-033)"; status=1; \
		fi; \
	done; \
	[ $$status -eq 0 ] || exit 1; \
	tracked=$$(git ls-tree -r --name-only HEAD -- specs .specify .opencode docs); \
	if [ -n "$$tracked" ]; then \
		echo 'FAIL: spec artifact tracked at HEAD (FR-033, NFR-008, SC-006):'; \
		echo "$$tracked"; \
		exit 1; \
	fi; \
	echo 'artifact-scope: no artifact path is tracked or unignored'

deps-root: ## Assert the root module has zero require directives and no go.sum
	@status=0; \
	if [ -f go.sum ]; then echo 'FAIL: root go.sum exists; the root module must have zero requires'; status=1; fi; \
	if grep -qE '^[[:space:]]*require' go.mod; then \
		echo 'FAIL: root go.mod contains a require directive — put the dependency in a nested module'; \
		grep -nE '^[[:space:]]*require' go.mod; status=1; \
	fi; \
	[ $$status -eq 0 ] || exit 1; \
	echo 'deps-root: root module has no third-party requirements'

deps: deps-root ## Probe dependency leakage with GOWORK=off in a throwaway consumer module
	@set -e; \
	tmp=$$(mktemp -d "$${TMPDIR:-/tmp}/qi-mcp-consumer.XXXXXX"); \
	trap 'rm -rf "$$tmp"' EXIT; \
	printf 'module probe\n\ngo %s\n\nrequire %s v0.0.0\n\nreplace %s => %s\n' \
		'$(GO_LANG)' '$(MODULE)' '$(MODULE)' '$(CURDIR)' > "$$tmp/go.mod"; \
	out=$$(cd "$$tmp" && GOWORK=off GOFLAGS=-mod=mod $(GO) list -m all); \
	echo "consumer resolves to:"; echo "$$out" | sed 's/^/  /'; \
	extra=$$(printf '%s\n' "$$out" | grep -v '^probe$$' | grep -vF '$(MODULE) v0.0.0 => ' || true); \
	if [ -n "$$extra" ]; then \
		echo 'FAIL: consumed module drags in unexpected requirements:'; \
		printf '%s\n' "$$extra"; exit 1; \
	fi; \
	echo 'deps: consumer module sees no leaked requirements'

tasks: ## Show the next unchecked task and how many remain (specs/.../tasks.md)
	@printf 'remaining  %s\n' "$$(grep -c '^- \[ \]' $(TASKS) || true)"
	@printf 'next       %s\n' "$$(grep -m1 '^- \[ \]' $(TASKS) || echo '(none — all tasks checked)')"

tidy: ## go mod tidy for the root module (keeps zero requires)
	$(GO) mod tidy
	@$(MAKE) --no-print-directory deps-root

clean: ## Remove local build outputs
	rm -f $(COVER_FILE) $(COVER_HTML) coverage.txt

clean-cache: ## Clear Go build/test caches and the linter cache
	$(GO) clean -cache -testcache
	$(GOLANGCI) cache clean