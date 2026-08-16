.PHONY: up down logs psql tidy test migrate migrate-down build dr-drill chaos-drill loadtest

COMPOSE := docker compose
GO_IMAGE := golang:1.25-bookworm
DEV_DB_URL := postgres://kolo:kolo@localhost:5432/kolo_bank?sslmode=disable

up:
	$(COMPOSE) up --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

psql:
	$(COMPOSE) exec postgres psql -U kolo -d kolo_bank

build:
	$(COMPOSE) build api

# go mod tidy runs in a throwaway container so go.mod/go.sum are generated
# without host Go (see docs/banking-backend-spec.md §9).
tidy:
	docker run --rm -v "$(CURDIR):/src" -w /src $(GO_IMAGE) go mod tidy

# Tests run against a real Postgres via docker-compose so migrations,
# constraints, and triggers are exercised, not just mocked out. -p 1 forces
# package test binaries to run one at a time rather than in parallel:
# internal/resilience's kill switches and read-only mode are genuine global
# singleton state in that one shared database (docs/banking-backend-spec.md
# §Phase 10), and several packages' tests deliberately flip real production
# scopes (e.g. "transfer") to prove the guard works — safe only if no other
# package's tests are concurrently relying on that scope being clear.
test:
	$(COMPOSE) up -d postgres
	docker run --rm --network kolo-bank-server_default \
		-v "$(CURDIR):/src" -w /src \
		-e DATABASE_URL="postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable" \
		$(GO_IMAGE) go test -p 1 ./...

# goose is installed fresh in a throwaway container rather than relying on a
# third-party goose image, keeping the toolchain reproducible from source.
GOOSE_RUN := docker run --rm --network kolo-bank-server_default \
	-v "$(CURDIR)/db/migrations:/migrations" \
	-e GOFLAGS=-mod=mod \
	$(GO_IMAGE) sh -c "go install github.com/pressly/goose/v3/cmd/goose@latest >/dev/null 2>&1 && goose -dir=/migrations postgres"

migrate:
	$(COMPOSE) up -d postgres
	$(GOOSE_RUN) "postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable" up

migrate-down:
	$(COMPOSE) up -d postgres
	$(GOOSE_RUN) "postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable" down

# DR drill (docs/dr-runbook.md): backup, restore into a scratch database,
# validate, fail over via a database rename + api restart, prove the
# restored copy is genuinely live, then clean up. Runs on the host (not
# containerized) since it orchestrates docker compose itself. Requires the
# stack to already be up (`make up`) and idle.
dr-drill:
	bash scripts/dr-drill.sh

# Chaos drill (docs/load-testing.md): kills the api and postgres containers
# mid-load and asserts the system recovers cleanly. Also host-run, for the
# same reason as dr-drill.
chaos-drill:
	bash scripts/chaos-drill.sh

# Load/soak test (docs/load-testing.md) against the running api container,
# from the same throwaway-container pattern as `test`/`migrate`.
loadtest:
	$(COMPOSE) up -d postgres
	docker run --rm --network kolo-bank-server_default \
		-v "$(CURDIR):/src" -w /src \
		-e DATABASE_URL="postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable" \
		$(GO_IMAGE) go run ./cmd/loadtest -base-url=http://api:8080 -duration=5m -concurrency=20
