# LLM-Wiki Demo

LLM-Wiki is a local demo whose center is compiled Wiki knowledge: `Document -> Understand -> Compile -> Wiki -> Retrieve`.

## Current scope

The demo preserves and parses Markdown, TXT, and text-based PDF sources, compiles them into traceable Wiki pages, indexes the compiled Wiki, and exposes Wiki browsing, hybrid retrieval, query, and multi-turn chat in one UI.

## Run

```sh
cp .env.example .env
make frontend-install
make dev
```

Backend: `http://127.0.0.1:8080/api/health`  
OpenAPI: `http://127.0.0.1:8080/docs`

LLM and embedding configuration is deliberately separate:

- Compiler, Query, and Chat use Baidu OneAPI through OpenAI-compatible Chat Completions. Configure `LLM_BASE_URL`, `ONEAPI_API_KEY`, and `LLM_MODEL` in `.env` or Settings. The default base URL is `https://oneapi-comate.baidu-int.com/v1`.
- Wiki and Source embeddings use local Ollama. Start Ollama, run `ollama pull qwen3-embedding:0.6b`, and configure `OLLAMA_BASE_URL` plus `EMBEDDING_MODEL`. Embeddings do not use an API key.
- `LLM_FAKE_FALLBACK=true` enables deterministic generation for explicit local demo/test use when OneAPI credentials are unavailable.

Provider status reports whether credentials are configured but never returns the OneAPI API key. Changing the embedding base URL or model marks active pages `index_pending`; run **Reindex now** in Settings before relying on vector search.

## Source API

Upload a supported source as multipart form data:

```sh
curl -F file=@notes.md http://127.0.0.1:8080/api/sources
```

Browse sources and evidence chunks at `GET /api/sources`, `GET /api/sources/{id}`, and `GET /api/sources/{id}/chunks`. Original files are preserved under `data/sources/original`; parsed projections are written under `data/sources/parsed`.

Compile a parsed source with `POST /api/compilations` and `{"document_id":"<source-id>"}`. Inspect the saved analysis, candidates, plan, validation, diff, and apply result at `GET /api/compilations/{run-id}`. A projection failure leaves a retryable `render_pending` run; retry it with `POST /api/compilations/{run-id}/render`.
