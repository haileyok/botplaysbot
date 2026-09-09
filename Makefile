ROOT_PACKAGE := github.com/haileyok/botplaysbot

# Default Postgres for `make test`/`make ci` (docker compose up -d db).
# Integration tests skip with a clear message when the database is absent.
DATABASE_URL ?= postgres://playsbot:playsbot@localhost:5432/playsbot?sslmode=disable
export DATABASE_URL

.DEFAULT_GOAL := help
.PHONY: help build test lint typecheck dev demo ci codegen codegen-check clean

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /make /'

## build: build the web frontend, then the Go binaries (with the fresh web build embedded)
build:
	pnpm install --frozen-lockfile
	pnpm --filter web build
	go build -o bin/ ./cmd/...

## test: run Go tests (unit + integration; integration skip cleanly when Postgres/dev PDS are unavailable)
test:
	go test -count=1 ./...

## lint: go vet + web eslint
lint:
	go vet ./...
	pnpm --filter web lint

## typecheck: compile Go tests + tsc for web
typecheck:
	go build ./...
	go test -run '^$$' -count=1 ./...
	pnpm --filter web typecheck

## dev: run the appview locally (placeholder; later phases add frontend watch)
dev:
	go run ./cmd/appview

## demo: run a demo stack (placeholder; later phases fill this in)
demo:
	@echo "demo: not implemented yet (Phase B+)"

## codegen: regenerate Go lexicon types from lexicons/ (atmos lexgen)
codegen:
	go generate ./internal/tools/gen

## codegen-check: fail if generated code does not match lexicons/
codegen-check:
	./scripts/check-codegen.sh

## ci: what CI runs: codegen freshness -> build -> typecheck -> lint -> test
ci: codegen-check build typecheck lint test

## clean: remove build artifacts
clean:
	rm -rf bin
