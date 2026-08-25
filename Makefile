GO ?= go
BINARY ?= bin/brclio-mail
VERSION ?= 0.1.0-preview
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: help fmt fmt-check third-party-notices third-party-notices-check test test-race vet vuln build ci docker-build compose-config run-dev doctor backup clean

help:
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_-]+:.*## / {printf "%-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format Go source files.
	$(GO) fmt ./...

fmt-check: ## Fail when a Go file is not gofmt formatted.
	@files="$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'))"; \
	test -z "$$files" || { printf '%s\n' "$$files"; exit 1; }

third-party-notices: ## Regenerate production-binary dependency notices.
	$(GO) run ./scripts/third-party-notices -output THIRD_PARTY_NOTICES

third-party-notices-check: ## Verify committed dependency notices are current.
	$(GO) run ./scripts/third-party-notices -output THIRD_PARTY_NOTICES -check

test: ## Run unit and integration tests.
	$(GO) test ./...

test-race: ## Run tests with the race detector.
	$(GO) test -race ./...

vet: ## Run go vet.
	$(GO) vet ./...

vuln: ## Run the current official govulncheck against reachable code.
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

build: ## Build the server binary.
	mkdir -p $(dir $(BINARY))
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/brclio-mail

ci: fmt-check third-party-notices-check test test-race vet vuln build ## Run the local equivalent of CI.

docker-build: ## Build the Preview container image.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) -t brclio-mail:preview .

compose-config: ## Validate the resolved Compose model.
	docker compose config --quiet

run-dev: ## Run a local web-only development instance.
	BRCLIO_DEV_MODE=true BRCLIO_DISABLE_MAIL_SERVERS=true BRCLIO_SETUP_TOKEN=development-only-token BRCLIO_HTTP_ADDR=127.0.0.1:8080 BRCLIO_BASE_URL=http://127.0.0.1:8080 $(GO) run ./cmd/brclio-mail serve

doctor: ## Inspect the configured local database.
	BRCLIO_DEV_MODE=true $(GO) run ./cmd/brclio-mail doctor

backup: ## Create a timestamped local SQLite backup under backups/.
	mkdir -p backups
	BRCLIO_DEV_MODE=true $(GO) run ./cmd/brclio-mail backup "backups/brclio-mail-$$(date -u +%Y%m%dT%H%M%SZ).sqlite"

clean: ## Remove the local build output only.
	rm -f $(BINARY)
