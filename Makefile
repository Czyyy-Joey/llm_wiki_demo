-include .env
APP_ADDR ?= 127.0.0.1:8080
DATABASE_URL ?= file:../data/app.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)
export

.PHONY: dev backend-test backend-build frontend-install frontend-check test

dev:
	@trap 'kill 0' EXIT; (cd backend && go run ./cmd/server) & (cd frontend && npm run dev)
backend-test:
	cd backend && go test ./...
backend-build:
	cd backend && go vet ./... && go build ./cmd/server
frontend-install:
	cd frontend && npm install
frontend-check:
	cd frontend && npm run lint && npm run typecheck && npm test -- --run
test: backend-test backend-build frontend-check
