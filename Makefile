.PHONY: generate build run tidy vet test docker

# Stamped into the binary, reported at startup and over /api/version. The
# appliance image build passes the release tag it publishes; a bare `make build`
# (or `go run`) reports "dev".
VERSION ?= dev
LDFLAGS := -X github.com/Venapce/venapce-api/internal/version.Version=$(VERSION)

# Regenerate the typed DB layer from internal/store/schema.sql + queries.
generate:
	sqlc generate

tidy:
	go mod tidy

vet:
	go vet ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/venapce-api ./cmd/api

# Run locally (expects a reachable Postgres — see .env.example).
run:
	go run ./cmd/api

test:
	go test ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t venapce-backend:latest .
