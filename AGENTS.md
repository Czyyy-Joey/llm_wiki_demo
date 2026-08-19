# Repository Guidelines

## Project Structure

The Go backend lives in `backend/`. HTTP wiring is under `backend/internal/api`, with domain contracts, source parsing, compilation, Wiki browsing, indexing, retrieval, query, and chat in their respective packages. Database migrations are in `backend/internal/db/migrations`; prompt files are in `backend/prompts`. Runtime originals, parsed artifacts, Wiki Markdown, and indexes belong under the configured `data/` root. The React and TypeScript client lives in `frontend/src`, with unit tests beside the client code and Playwright scenarios in `frontend/tests/e2e`.

## Build, Test, and Development

Run the full local stack with `make dev`. Backend checks are:

```bash
cd backend
gofmt -w ./cmd ./internal
go test ./...
go vet ./...
go build ./cmd/server
```

Frontend checks are run from `frontend/`: `npm run lint`, `npm run typecheck`, `npm test -- --run`, and `npm run build`. Use `npm run e2e` for the real upload-to-chat demo; it starts isolated backend and Vite servers.

## Coding Style

Use `gofmt` and idiomatic Go naming. Keep package boundaries explicit and put shared request/response contracts in the domain or API contract layers. TypeScript uses two-space indentation, single quotes, and semicolons omitted, matching the existing ESLint configuration. React components use PascalCase filenames; hooks and helpers use camelCase. Keep user-facing UI states explicit: loading, empty, error, retry, and success.

## Testing Guidelines

Go tests use the standard `testing` package and should use temporary SQLite/data roots. Name tests by behavior, such as `TestCompileCreatesThenUpdates`. Vitest covers frontend contracts and components; Playwright covers real browser workflows and must remain under `tests/e2e` so Vitest does not collect it.

## Commits and Pull Requests

Use concise imperative commit subjects with a category prefix, following existing history such as `feat: complete phase 3 wiki services and phase 4 hybrid retrieval`. Pull requests should describe behavior and scope, include the commands run, call out schema or migration changes, and attach screenshots or a short flow recording for visible UI changes. Keep Phase boundaries explicit and avoid unrelated infrastructure.

## Configuration and Security

Copy `.env.example` for local settings. Keep the backend bound to `127.0.0.1:8080` by default, use fake providers only for development/tests, and never commit API keys or generated `data/` artifacts.
