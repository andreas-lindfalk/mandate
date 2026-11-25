.PHONY: up down reset gen web web-dev test lint run migrate demo tidy check

COMPOSE := docker compose -f deploy/compose.yaml

up: ## Start Postgres locally
	$(COMPOSE) up -d --wait postgres

down: ## Stop local infrastructure
	$(COMPOSE) down

reset: ## Wipe the local database and start fresh
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait postgres

gen: ## Regenerate Connect, protobuf and sqlc code
	buf lint
	buf generate
	sqlc generate

web: ## Build the web app into web/dist (embedded in the binary)
	cd web && pnpm install --frozen-lockfile && pnpm build

web-dev: ## Vite dev server on :5173 with hot reload, proxying the API to :8585
	cd web && pnpm dev

test: ## Run all tests (needs Docker for testcontainers)
	go test -p 4 -timeout 600s ./...

lint: ## Lint like CI does
	golangci-lint run ./...

run: web ## Build the web app and serve the UI, the API and MCP on 127.0.0.1:8585 with dev login (migrates on start)
	MANDATE_DEV_LOGIN=true MANDATE_ALLOW_PRIVATE_CONNECTORS=true go run ./cmd/pgmandate serve

migrate:
	go run ./cmd/pgmandate migrate

demo: ## Play the five-minute demo against the local Postgres
	go run ./cmd/pgmandate demo

tidy:
	go mod tidy

check: gen tidy lint test web ## Everything CI checks
	git diff --exit-code -- '*/internal/repo/*' go.mod go.sum gen web/src/gen
