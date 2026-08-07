.PHONY: help build run test lint migrate tidy

APP := fleet-tenancy-api

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

COMMIT ?= dev

build: ## Build the binary into target/bin
	@mkdir -p target/bin
	CGO_ENABLED=0 go build -ldflags "-X main.commitHash=$(COMMIT)" -o target/bin/$(APP) ./cmd/$(APP)

run: ## Run the server
	go run ./cmd/$(APP)

test: ## Run tests
	go test ./...

lint: ## Vet and format-check
	go vet ./...
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt needed"; exit 1)

migrate: ## Apply database migrations
	go run ./cmd/$(APP) migrate -up

tidy: ## Tidy modules
	go mod tidy
