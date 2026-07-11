# Kanz developer task targets (DEVX-01e). Replaces the hand-run buf-generate
# flow the team kept rediscovering (regenerate the SDK, remember -mod=mod, the
# protoc-gen-go-grpc plugin). `make help` lists everything; `make bootstrap`
# then `make generate test` is the from-scratch path. See docs/onboarding.md.

# The kanz module pins -mod=mod so a stale gen/ SDK is regenerated rather than
# failing the build (the EVT-15a generated-not-committed stance).
export GOFLAGS := -mod=mod

# Windows Application Control intermittently blocks freshly built test binaries
# in %TEMP%, which makes `go test` fail at random with "Uygulama Denetimi ilkesi
# bu dosyayı engelledi". Building them under a project-local dir is not blocked,
# so every target that compiles a binary points GOTMPDIR here. Exported (not
# per-target) so it holds for an ad-hoc `go test` run from this directory too.
export GOTMPDIR := $(CURDIR)/.gotmp

# The exchange connectors are compiled in behind build tags; the default build is
# vendor-free. VENUE_TAGS is what the *-venues targets link.
VENUE_TAGS := binance okx

SCHEMAS_DIR := ../kanz-schemas
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## Install the codegen tools (buf, protoc-gen-go, protoc-gen-go-grpc)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@command -v buf >/dev/null || echo "install buf: https://buf.build/docs/installation"

.PHONY: generate
generate: ## Regenerate the kanz-schemas SDKs (buf generate)
	cd $(SCHEMAS_DIR) && buf generate

.PHONY: build
build: ## Build every package
	go build ./...

# Production release build (IP-security mandate). -ldflags="-s -w" strips the
# symbol table (-s) and DWARF debug info (-w) so the shipped binary resists
# decompilation of the proprietary execution logic; -trimpath removes local
# filesystem paths from the binary. Exchange connectors are linked in via TAGS
# (e.g. `make release TAGS="binance okx"`); the default binary stays vendor-free.
# Secrets never enter the binary — keys are fetched at runtime into RAM via the
# env / mock-Vault (`*_FILE`) boundary, never hardcoded or committed.
GO_RELEASE_LDFLAGS := -s -w
TAGS ?=

.PHONY: release
release: ## Build stripped, trimpath production binaries into bin/ (TAGS="binance okx" to link venues)
	@mkdir -p bin
	go build -trimpath -ldflags="$(GO_RELEASE_LDFLAGS)" -tags "$(TAGS)" -o bin/ ./services/...

# NOTE: -race requires cgo (CGO_ENABLED=1). On a box without a C toolchain it
# fails with "-race requires cgo"; run `make test` there and `make test-race`
# where cgo is available (CI).
.PHONY: test
test: | $(GOTMPDIR) ## Run the test suite (default, vendor-free build)
	go test ./...

.PHONY: test-race
test-race: | $(GOTMPDIR) ## Run the test suite with the race detector (needs CGO_ENABLED=1)
	go test -race ./...

.PHONY: test-venues
test-venues: | $(GOTMPDIR) ## Run the test suite with the exchange connectors linked
	go test -tags "$(VENUE_TAGS)" ./...

.PHONY: build-venues
build-venues: ## Build every package with the exchange connectors linked
	go build -tags "$(VENUE_TAGS)" ./...

$(GOTMPDIR):
	@mkdir -p $(GOTMPDIR)

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## gofmt-check every Go file (fails if any need formatting)
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

.PHONY: lint
lint: vet fmt ## vet + gofmt check

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: scaffold
scaffold: ## Scaffold a new service: make scaffold NAME=my-service
	@test -n "$(NAME)" || (echo "usage: make scaffold NAME=my-service" && exit 2)
	go run ./tools/scaffold/cmd/scaffold -name $(NAME)

.PHONY: up
up: ## Bring up the local dev stack (NATS+Kafka+Postgres+services+seed)
	docker compose -f dev/docker-compose.yml up -d --build
	go run ./test/load/seed || true

.PHONY: down
down: ## Tear down the local dev stack
	docker compose -f dev/docker-compose.yml down -v

.PHONY: ci
ci: generate lint build test ## What CI runs: regenerate, lint, build, test
