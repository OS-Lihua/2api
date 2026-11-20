WEB_DIR = ./web
API_DIR = .
DEV_WEB_PORT ?= 5173
DEV_COMPOSE_FILE = docker-compose.dev.yml
DEV_POSTGRES_SERVICE = postgres
DEV_API_SERVICE = new-api
DEV_POSTGRES_DB = new-api
DEV_POSTGRES_USER = root

.PHONY: all build-web build-all-web start-api dev dev-api dev-api-rebuild dev-web reset-setup \
	test test-modules test-router-native test-router-text test-router-other

all: build-all-web start-api

build-web:
	@echo "Building web frontend..."
	@cd $(WEB_DIR) && bun install --frozen-lockfile
	@cd $(WEB_DIR) && DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$$(cat ../VERSION) bun run build

build-all-web: build-web

start-api:
	@echo "Starting api dev server..."
	@cd $(API_DIR) && go run main.go &

dev-api:
	@echo "Starting api services (docker)..."
	@docker compose -f $(DEV_COMPOSE_FILE) up -d

dev-api-rebuild:
	@echo "Rebuilding and starting api service (docker)..."
	@docker compose -f $(DEV_COMPOSE_FILE) up -d --build $(DEV_API_SERVICE)

dev-web:
	@echo "Starting web frontend dev server..."
	@echo "Web frontend: http://localhost:$(DEV_WEB_PORT)"
	@cd $(WEB_DIR) && bun install
	@cd $(WEB_DIR) && bun run dev -- --host 0.0.0.0 --port $(DEV_WEB_PORT)

dev: dev-api dev-web

ROUTER_NATIVE_TESTS = TestResponsesWS|TestRealtime|TestWebSocket|TestNativeRouteCredentialForms
ROUTER_TEXT_TESTS = TestTextGateway

test:
	$(MAKE) test-modules
	$(MAKE) test-router-native
	$(MAKE) test-router-text
	$(MAKE) test-router-other

# The main package embeds the ignored web/dist output and is covered after build-web.
test-modules:
	@echo "Testing root Go module..."
	@set -eu; \
		root_module=$$(GOWORK=off go list -m); \
		root_packages=$$(GOWORK=off go list -e ./... | grep -vxF -e "$$root_module" -e "$$root_module/router"); \
		GOWORK=off go test -timeout=40m $$root_packages
	@echo "Testing relaykit Go module..."
	@cd relaykit && GOWORK=off go test ./...
	@echo "Testing tokenkit Go module..."
	@cd tokenkit && GOWORK=off go test ./...

test-router-native:
	GOWORK=off go test -timeout=40m -run '^($(ROUTER_NATIVE_TESTS))' ./router

test-router-text:
	GOWORK=off go test -timeout=70m -run '^$(ROUTER_TEXT_TESTS)' ./router

test-router-other:
	GOWORK=off go test -timeout=70m -skip '^($(ROUTER_NATIVE_TESTS)|$(ROUTER_TEXT_TESTS))' ./router

reset-setup:
	@echo "Resetting local setup wizard state..."
	@if docker compose -f $(DEV_COMPOSE_FILE) ps --services --status running | grep -qx "$(DEV_POSTGRES_SERVICE)"; then \
		echo "Detected running docker dev PostgreSQL. Removing setup record and root users..."; \
		docker compose -f $(DEV_COMPOSE_FILE) exec -T $(DEV_POSTGRES_SERVICE) \
			psql -U $(DEV_POSTGRES_USER) -d $(DEV_POSTGRES_DB) \
			-c 'DELETE FROM setups;' \
			-c 'DELETE FROM users WHERE role = 100;' \
			-c "DELETE FROM options WHERE key IN ('SelfUseModeEnabled', 'DemoSiteEnabled');"; \
		echo "Restarting docker dev api so setup status is recalculated..."; \
		docker compose -f $(DEV_COMPOSE_FILE) restart $(DEV_API_SERVICE); \
	else \
		echo "No running docker dev PostgreSQL found."; \
		echo "Start the dev stack with 'make dev-api'."; \
		exit 1; \
	fi
