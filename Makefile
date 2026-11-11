# Ferry — developer commands. Run `make help` for a list.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# Load .env if present (git-ignored); fall back to .env.example defaults.
ifneq (,$(wildcard .env))
include .env
export
endif

POSTGRES_USER     ?= ferry
POSTGRES_PASSWORD ?= ferry
POSTGRES_DB       ?= ferry
POSTGRES_PORT     ?= 5432
DATABASE_URL      ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

# Pinned tool versions. Tools are installed into ./bin (git-ignored) rather than
# go.mod so they don't pull their dependency trees into the module graph.
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
GITLEAKS_VERSION      := v8.30.1
GOOSE_VERSION         := v3.29.0
SQLC_VERSION          := v1.31.1

BIN := $(CURDIR)/bin
export PATH := $(BIN):$(PATH)

.PHONY: help up obs-up alerts-check down migrate migrate-status run relay worker recon fake-noah watcher watcher-solana contracts chain-setup evm-setup solana-setup test lint vet vuln gitleaks sqlc tools

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

up: ## Start local infra (Postgres, queue, local chains) and wait until healthy
	@mkdir -p .dev/solana  # before Docker would create it as root for the bind mount
	docker compose up -d --wait

obs-up: ## Start Prometheus (:9090), Grafana (:3000) and Jaeger (:16686), all on 127.0.0.1
	docker compose --profile observability up -d

alerts-check: ## Validate the Prometheus config and alert rules with promtool
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.15.1 \
		check config /etc/prometheus/prometheus.yml

down: ## Stop local infra and delete its volumes
	docker compose down -v

migrate: $(BIN)/goose ## Apply migrations to DATABASE_URL
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-status: $(BIN)/goose ## Show migration status
	goose -dir migrations postgres "$(DATABASE_URL)" status

run: ## Run the API (in dev also the relay, worker and resolver; needs `make up`)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api

relay: ## Run the outbox relay (needs `make up` for the queue)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/relay

worker: ## Run the payout worker and resolver (FERRY_RAIL=noah; needs `make up`)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/worker

recon: ## Run reconciliation once (chains and rail as configured in .env)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/recon

fake-noah: ## Run a fake Noah API on 127.0.0.1:18090 for local payouts without a sandbox key
	go run ./internal/rail/noah/noahtest/cmd/fakenoah

watcher: ## Run the EVM deposit watcher (needs `make up` and `make chain-setup`)
	DATABASE_URL="$(DATABASE_URL)" FERRY_CHAIN=evm go run ./cmd/watcher

watcher-solana: ## Run the Solana deposit watcher (needs `make up` and `make chain-setup`)
	DATABASE_URL="$(DATABASE_URL)" FERRY_CHAIN=solana go run ./cmd/watcher

# Anvil's first default account. Anvil keeps its keys unlocked, so no
# private key is needed (or stored) to deploy or send from it.
ANVIL_ACCOUNT := 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
SOLC_IMAGE    := ethereum/solc:0.8.30
ANVIL_CAST    := docker compose exec -T anvil cast send --rpc-url http://localhost:8545 --unlocked --from $(ANVIL_ACCOUNT)

contracts: ## Recompile contracts/ with the pinned solc image
	docker run --rm -v "$(CURDIR)/contracts:/src" -u "$$(id -u):$$(id -g)" $(SOLC_IMAGE) \
		--optimize --bin --abi --overwrite -o /src/build /src/TestUSDC.sol

chain-setup: evm-setup solana-setup ## Set up both local chains (test USDC on Anvil and on the Solana validator)

evm-setup: ## Deploy TestUSDC to local Anvil and mint 1,000,000 to the first account
	@addr=$$($(ANVIL_CAST) --json --create $$(cat contracts/build/TestUSDC.bin) \
		| tr ',' '\n' | grep '"contractAddress"' | cut -d'"' -f4); \
	$(ANVIL_CAST) $$addr "mint(address,uint256)" $(ANVIL_ACCOUNT) 1000000000000 >/dev/null; \
	echo "TestUSDC deployed at $$addr (FERRY_EVM_USDC_ADDRESS)"

# Runs inside the validator container, which has the solana and spl-token
# CLIs. Keypairs are created once in .dev/solana (git-ignored) and reused,
# so the mint and authority addresses stay the same across chain resets.
#   authority.json  Ferry's authority (FERRY_SOLANA_AUTHORITY_KEYPAIR), 10 SOL for rent and fees
#   owner.json      the deposit owner: token owner of every deposit account. Ferry gets only its
#                   public key (FERRY_SOLANA_DEPOSIT_OWNER); the private key is for the future sweep
#   wallet.json     a dev sender: mint authority, holds 1,000,000 test USDC
#   usdc-mint.json  the test USDC mint (6 decimals)
SOLANA_EXEC := docker compose exec -T solana
solana-setup: ## Create the test USDC mint on the local Solana validator and fund the dev keys
	@mkdir -p .dev/solana
	@$(SOLANA_EXEC) sh -ec '\
		cd /ferry; \
		for k in authority owner wallet usdc-mint; do [ -f $$k.json ] || solana-keygen new --no-bip39-passphrase --silent -o $$k.json >/dev/null; done; \
		chmod 600 *.json; chown $(shell id -u):$(shell id -g) . *.json; \
		solana config set -u http://localhost:8899 -k /ferry/wallet.json >/dev/null; \
		solana airdrop 100 >/dev/null; \
		solana airdrop 10 authority.json >/dev/null; \
		spl-token create-token --decimals 6 usdc-mint.json >/dev/null; \
		mint=$$(solana-keygen pubkey usdc-mint.json); \
		spl-token create-account $$mint >/dev/null; \
		spl-token mint $$mint 1000000 >/dev/null; \
		echo "Solana test USDC mint $$mint (FERRY_SOLANA_USDC_MINT)"; \
		echo "Solana authority $$(solana-keygen pubkey authority.json) (FERRY_SOLANA_AUTHORITY)"; \
		echo "Solana deposit owner $$(solana-keygen pubkey owner.json) (FERRY_SOLANA_DEPOSIT_OWNER)"'

test: ## Unit + integration tests with the race detector
	go test -race -count=1 ./...

vet: ## go vet
	go vet ./...

# gosec runs inside golangci-lint (see .golangci.yml), with full type information.
lint: vet $(BIN)/golangci-lint vuln ## go vet + golangci-lint (incl. gosec) + govulncheck
	golangci-lint run

vuln: $(BIN)/govulncheck ## Known-vulnerability scan of dependencies
	govulncheck ./...

gitleaks: $(BIN)/gitleaks ## Scan git history and working tree for secrets
	gitleaks git --redact --no-banner .
	gitleaks dir --redact --no-banner .

sqlc: $(BIN)/sqlc ## Regenerate query code
	sqlc generate

tools: $(BIN)/golangci-lint $(BIN)/govulncheck $(BIN)/gitleaks $(BIN)/goose $(BIN)/sqlc ## Install all pinned tools into ./bin

$(BIN)/golangci-lint:
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
$(BIN)/govulncheck:
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
$(BIN)/gitleaks:
	GOBIN=$(BIN) go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
$(BIN)/goose:
	GOBIN=$(BIN) go install github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)
$(BIN)/sqlc:
	GOBIN=$(BIN) go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
