# LLM-Wiki Demo

LLM-Wiki is a local demo whose center is compiled Wiki knowledge: `Document -> Understand -> Compile -> Wiki -> Retrieve`.

## Current scope

Phase 2 preserves and parses Markdown, TXT, and text-based PDF sources, then compiles structured analysis into traceable Wiki pages through `Parse -> Analyze -> Match -> Plan -> Validate -> Apply`. Indexing, retrieval, query, chat, and the full Wiki browsing UI remain reserved for later phases.

## Run

```sh
cp .env.example .env
make frontend-install
make dev
```

Backend: `http://127.0.0.1:8080/api/health`  
OpenAPI: `http://127.0.0.1:8080/docs`

LLM and embedding credentials are optional in Phase 0. The status endpoints explicitly report whether each provider is configured.

Knowledge compilation uses an OpenAI-compatible Chat Completions provider when `COMPILER_LLM_ENDPOINT`, `COMPILER_LLM_API_KEY`, and `COMPILER_LLM_MODEL` are configured. For local development and tests only, set `COMPILER_LLM_FAKE_FALLBACK=true` to enable the deterministic fake adapter.

## Source API

Upload a supported source as multipart form data:

```sh
curl -F file=@notes.md http://127.0.0.1:8080/api/sources
```

Browse sources and evidence chunks at `GET /api/sources`, `GET /api/sources/{id}`, and `GET /api/sources/{id}/chunks`. Original files are preserved under `data/sources/original`; parsed projections are written under `data/sources/parsed`.

Compile a parsed source with `POST /api/compilations` and `{"document_id":"<source-id>"}`. Inspect the saved analysis, candidates, plan, validation, diff, and apply result at `GET /api/compilations/{run-id}`. A projection failure leaves a retryable `render_pending` run; retry it with `POST /api/compilations/{run-id}/render`.
