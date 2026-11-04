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

.PHONY: help up down migrate migrate-status run test lint vet vuln gitleaks sqlc tools

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

up: ## Start local infra (Postgres) and wait until healthy
	docker compose up -d --wait

down: ## Stop local infra and delete its volumes
	docker compose down -v

migrate: $(BIN)/goose ## Apply migrations to DATABASE_URL
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-status: $(BIN)/goose ## Show migration status
	goose -dir migrations postgres "$(DATABASE_URL)" status

run: ## Run the API server (reads .env; see .env.example)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api

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
