.PHONY: up down reset gen web web-dev test lint run migrate demo demo-terminal-gif demo-ui-gifs tidy check

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

demo: ## Play the demo against the local Postgres (a little over a minute)
	go run ./cmd/pgmandate demo

demo-terminal-gif: ## Record the terminal film into docs/terminal.gif (needs make up, vhs and tmux; see docs/film/terminal.tape)
	vhs docs/film/terminal.tape

demo-ui-gifs: ## Record the UI films into docs/ui-*.gif: after make demo, with make run and the demo's receiver running, MANDATE_AGENT_TOKEN=<the token it printed> make demo-ui-gifs (see web/scripts/record-ui.mjs)
	cd web && node scripts/record-ui.mjs

tidy:
	go mod tidy

check: gen tidy lint test web ## Everything CI checks
	git diff --exit-code -- '*/internal/repo/*' go.mod go.sum gen web/src/gen
