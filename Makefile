.PHONY: up down logs psql tidy test migrate migrate-down build

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
# constraints, and triggers are exercised, not just mocked out.
test:
	$(COMPOSE) up -d postgres
	docker run --rm --network kolo-bank-server_default \
		-v "$(CURDIR):/src" -w /src \
		-e DATABASE_URL="postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable" \
		$(GO_IMAGE) go test ./...

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
