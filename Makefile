.PHONY: help setup test lint run build db-up db-down migrate test-db selftest

# Default target — show help
help:
	@echo "tugbot (Go) Makefile"
	@echo ""
	@echo "Available targets:"
	@echo "  make setup      - Set up development environment (start DB, run migrations)"
	@echo "  make test       - Run all tests"
	@echo "  make lint       - Run linting (golangci-lint + go vet)"
	@echo "  make run        - Run the bot locally"
	@echo "  make build      - Build the project (debug mode)"
	@echo "  make db-up      - Start the PostgreSQL container (compose service 'postgres', postgres:postgres@localhost:5432/tugbot)"
	@echo "  make db-down    - Stop the PostgreSQL container"
	@echo "  make migrate    - Run database migrations (DATABASE_URL must be set)"
	@echo "  make test-db    - Start the compose PG, run the DB-touching tests, stop the PG"
	@echo "  make selftest   - Build and run `tugbot --selftest` (CI: config/pool/session/handlers constructed; no gateway; needs `make db-up` first)"

# Set up development environment
setup: db-up migrate
	@echo "Development environment ready!"

# Run all tests
test:
	go test ./...

# Run linting (golangci-lint + go vet)
lint:
	golangci-lint run
	go vet ./...

# Run the bot locally
run:
	go run ./cmd/tugbot

# Build the project
build:
	go build -o tugbot ./cmd/tugbot

# Stop / start the PostgreSQL container (the compose service name is 'postgres')
db-down:
	docker compose down

db-up:
	docker compose up -d postgres

# Run database migrations (reads DATABASE_URL, the compose default is
# postgres://postgres:postgres@localhost:5432/tugbot)
migrate:
	go run ./cmd/migrate

# Start the compose PG, run the DB-touching tests against it (the DB's
# own credentials are used via TUGBOT_TEST_DATABASE_URL), stop the PG
test-db: db-up
	TUGBOT_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/tugbot go test ./internal/dbmigrate ./internal/features -count=1
	docker compose down

# Build, then the CI selftest surface (the selftest connects to the
# compose PG — the compose name in the --selftest flag's help text;
# `make db-up` is the equivalent of starting it, first stop the instance)
selftest: build
	./tugbot --selftest
