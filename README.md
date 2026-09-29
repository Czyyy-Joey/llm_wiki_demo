# LLM-Wiki

> Turn raw documents into a **compiled, traceable knowledge Wiki** — then browse, retrieve, and chat over it.

LLM-Wiki is a local full-stack demo built around a single idea: knowledge should be *compiled*, not just chunked and embedded.

```text
Document  →  Understand  →  Compile  →  Wiki  →  Retrieve
```

Instead of the traditional RAG path (`Document → Chunk → Embedding → Retrieve`), every source is read and reorganized by an LLM into human-readable Wiki pages that stay traceable back to the original text. Compiled Wiki sits at the center; vector search and Wiki links are the retrieval mechanism on top of it.

---

## Highlights

- **Knowledge Compiler** — a plan-then-write pipeline (`Analyze → Match existing Wiki → Plan → Validate → Apply`) where the LLM makes semantic decisions and code executes them deterministically.
- **Incremental integration** — new documents update, merge, or link into existing pages (`CREATE / UPDATE / MERGE / LINK / NO-OP`) instead of piling up duplicates.
- **Full traceability** — Wiki claims cite the source chunks they came from; original files are always preserved as ground truth.
- **Hybrid retrieval** — lexical (SQLite FTS5) + vector search fused with RRF, expanded through Wiki links, then reranked into a budgeted context.
- **Observable at every step** — inspect the compilation plan, the resulting Wiki diff, and the retrieval candidates → expansion → rerank → context flow.
- **One UI** — upload sources, watch compilation, browse the Wiki, and run query/chat in a single React app.

## Architecture

```text
                React Web UI
                     │  REST / SSE
        ┌────────────┴─────────────┐
        │      Go HTTP Server      │
        ├──────────────────────────┤
        │ Source Service           │  Upload → Preserve → Parse → Chunks
        │ Knowledge Compiler       │  Analyze → Match → Plan → Validate → Apply
        │ Wiki Service             │  Pages · Sections · Claims · Links · Revisions
        │ Indexer                  │  SQLite FTS5 + vector index
        │ Retrieval                │  Lexical + Vector → RRF → Link Expansion → Rerank
        │ Query / Chat             │  Context → Answer → Citations → Conversation
        └──────────────────────────┘
                     │
        SQLite (state) · Markdown (Wiki) · rebuildable indexes
```

Each module is decoupled and independently replaceable. SQLite holds normalized run state, Markdown holds the human-readable Wiki projection, and all indexes are derived data that can be rebuilt.

## Tech stack

| Layer     | Stack                                                        |
| --------- | ------------------------------------------------------------ |
| Backend   | Go 1.23, SQLite (FTS5), OpenAI-compatible Chat Completions    |
| Frontend  | React 18, TypeScript, Vite                                   |
| Embeddings| Local Ollama (`qwen3-embedding:0.6b`)                        |
| Testing   | Go `testing`, Vitest, Playwright                             |

## Quick start

```sh
cp .env.example .env      # then fill in your OneAPI key / model
make frontend-install
make dev
```

- App API health: <http://127.0.0.1:8080/api/health>
- OpenAPI docs: <http://127.0.0.1:8080/docs>
- Frontend dev server is started by `make dev` alongside the backend.

## Configuration

LLM generation and embeddings are configured separately — set these in `.env` or in the in-app **Settings** panel.

| Variable          | Purpose                                        | Default                                   |
| ----------------- | ---------------------------------------------- | ----------------------------------------- |
| `APP_ADDR`        | Backend bind address                           | `127.0.0.1:8080`                          |
| `LLM_BASE_URL`    | OpenAI-compatible endpoint (compiler/query/chat)| `https://oneapi-comate.baidu-int.com/v1` |
| `ONEAPI_API_KEY`  | API key for the LLM endpoint                   | _(required for real generation)_          |
| `LLM_MODEL`       | Chat/completion model name                     | _(set per your provider)_                 |
| `LLM_FAKE_FALLBACK`| Deterministic offline generation for demos/tests | `true`                                 |
| `OLLAMA_BASE_URL` | Local Ollama endpoint for embeddings           | `http://127.0.0.1:11434`                  |
| `EMBEDDING_MODEL` | Embedding model (no API key needed)            | `qwen3-embedding:0.6b`                    |

Notes:
- Prepare embeddings with `ollama pull qwen3-embedding:0.6b` before relying on vector search.
- Provider status reports *whether* credentials are configured but **never returns the API key**.
- Changing the embedding base URL or model marks active pages `index_pending`; run **Reindex now** in Settings before trusting vector results.

> **Security:** keep the backend bound to `127.0.0.1` for local use, use `LLM_FAKE_FALLBACK` for offline demos, and never commit `.env` or generated `data/` artifacts. Only `.env.example` (with empty secrets) is checked in.

## API tour

Upload a supported source (Markdown, TXT, or text-based PDF):

```sh
curl -F file=@notes.md http://127.0.0.1:8080/api/sources
```

| Action            | Endpoint                                             |
| ----------------- | ---------------------------------------------------- |
| List / inspect    | `GET /api/sources`, `GET /api/sources/{id}`          |
| Evidence chunks   | `GET /api/sources/{id}/chunks`                       |
| Compile a source  | `POST /api/compilations` `{"document_id":"<id>"}`    |
| Inspect a run     | `GET /api/compilations/{run-id}`                     |
| Retry a projection| `POST /api/compilations/{run-id}/render`             |

Original files are preserved under `data/sources/original`; parsed projections under `data/sources/parsed`. A failed projection leaves a retryable `render_pending` run.

## Project layout

```text
backend/
  cmd/server/            HTTP entrypoint
  internal/
    api/                 HTTP wiring
    sources/ parsers/    upload, preserve, parse
    compiler/            knowledge compilation pipeline
    wiki/                pages, sections, claims, links, revisions
    indexing/ retrieval/ FTS5 + vector index, hybrid retrieval
    query/ chat/         answering and conversations
    db/migrations/       schema migrations
  prompts/               compiler & chat prompt files
frontend/
  src/features/          sources, compilation, wiki, search, query, chat, settings, graph
  tests/e2e/             Playwright scenarios
data/                    runtime originals, parsed artifacts, Wiki Markdown, indexes
```

## Development

```sh
make dev              # run backend + frontend together

# Backend checks
cd backend && gofmt -w ./cmd ./internal && go test ./... && go vet ./... && go build ./cmd/server

# Frontend checks (from frontend/)
npm run lint && npm run typecheck && npm test -- --run && npm run build
npm run e2e           # full upload-to-chat browser flow
```

See [AGENTS.md](AGENTS.md) for repository conventions, coding style, and contribution guidelines.
