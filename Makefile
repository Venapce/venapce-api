.PHONY: generate build run tidy vet test docker

# Regenerate the typed DB layer from internal/store/schema.sql + queries.
generate:
	sqlc generate

tidy:
	go mod tidy

vet:
	go vet ./...

build:
	go build -o bin/venapce-api ./cmd/api

# Run locally (expects a reachable Postgres — see .env.example).
run:
	go run ./cmd/api

test:
	go test ./...

docker:
	docker build -t venapce-backend:latest .
