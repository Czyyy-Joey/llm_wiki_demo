# LLM-Wiki Demo Development Plan

## 1. 文档目的

本文档定义 `llm_wiki_demo` 的可执行开发计划。

项目目标是实现一个可以本地运行和演示的全栈 LLM-Wiki / RAG / Knowledge System：

```text
Raw Documents
    ↓
Karpathy-style Knowledge Compilation
    ↓
Compiled Wiki
    ↓
Vector Search + Wiki Structure
    ↓
Query / Chat
```

本计划以 `DESIGN_PRINCIPLES.md` 为最高优先级。系统必须以 Compiled Wiki 为核心知识表示，不能退化为“上传、切块、向量检索、回答”的传统 RAG。

## 2. 当前仓库状态

当前仓库尚无应用代码，已有：

```text
DESIGN_PRINCIPLES.md
```

因此，本项目可以从清晰的模块边界和数据契约开始构建，不需要兼容已有实现。

## 3. 参考仓库分析

### 3.1 OpenKB

#### 值得借鉴

- 将 Raw Sources、Compiled Wiki 和生成能力分层。
- 区分单文档 Summary 与跨文档 Concept/Entity 页面。
- 编译新文档前读取已有 Wiki，并优先更新已有页面。
- 使用 Markdown、frontmatter 和 wikilink 表达可读、可迁移的 Wiki。
- 由代码维护 index、frontmatter、日志和文件写入等确定性状态。
- 提供事务写入、结构 lint、SHA256 去重和失败恢复。
- Query 和 Chat 都建立在已经编译的 Wiki 之上。

#### 不适合本 Demo

- PageIndex 和 vectorless reasoning 是其核心检索路线，与本项目必须使用向量搜索的要求冲突。
- Skill Factory、Deck、多模态、通用 Agent 和复杂 CLI 超出 Demo 范围。
- Query 过度依赖 Agent 自主选择和读取文件，检索过程不够确定、难以统一评估。
- 编译器已经承担大量格式、Provider 和生产环境兼容逻辑，不适合直接移植。

### 3.2 llm_wiki

#### 值得借鉴

- 使用 Analyze -> Generate 的两阶段摄入流程。
- 新知识会更新和融合已有页面，而不是只生成独立 Summary。
- 使用 `sources` frontmatter 累积页面来源。
- 通过关键词、向量和 Wiki 链接扩展进行 Hybrid Retrieval。
- 使用 RRF 合并不同检索信号。
- 对上下文长度进行预算控制。
- 展示摄入进度、检索过程和失败状态。
- 对模型生成结果进行校验，并在合并失败时保护已有页面。

#### 不适合本 Demo

- Tauri、Rust、React 的多运行时架构增加了本地 Demo 的开发和调试成本。
- 桌面封装、知识图社区分析、Deep Research 和完整 Review 系统范围过大。
- 让模型输出自定义 `FILE` 文本块再解析，协议脆弱且不利于严格校验。
- Provider 特殊适配和大型摄入队列属于后续生产化能力。

### 3.3 本项目的综合取舍

本项目借鉴两个参考仓库的 Knowledge Compilation 思想、增量融合、来源追踪和结构检索，但采用更小、更清晰的实现：

- LLM 只负责语义分析和提出结构化 Compilation Plan。
- 代码负责候选召回、schema 校验、事务 Apply、版本记录、索引更新和 Markdown 渲染。
- 使用向量检索寻找语义入口，同时使用 Wiki 链接补充结构上下文。
- SQLite 保存规范化运行状态，Markdown 保存人类可读的 Wiki 投影。
- 所有索引均为可重建的派生数据。

## 4. 总体架构

```text
React Web UI
      ↓ REST / SSE
Go HTTP Application
 ├─ Source Service
 │    Upload -> Preserve -> Parse -> SourceDocument / SourceChunk
 │
 ├─ Knowledge Compiler
 │    Analyze -> Match Existing Wiki -> Plan -> Validate -> Apply
 │
 ├─ Wiki Service
 │    Pages -> Sections -> Claims -> Links -> Revisions -> Markdown
 │
 ├─ Indexer
 │    SQLite FTS5 + chromem-go
 │
 ├─ Retrieval
 │    Lexical + Vector -> RRF -> Wiki Link Expansion -> Rerank -> Context
 │
 └─ Query / Chat
      Context -> LLM Answer -> Citation -> Persisted Conversation
```

### 4.1 模块边界

```text
Source Parser
    ↓
Knowledge Compiler
    ↓
Wiki Repository
    ↓
Indexer
    ↓
Retriever
    ↓
Query / Chat
```

模块之间通过明确的 Go DTO、JSON Schema 和领域类型交互，不共享未经校验的 LLM 输出。

### 4.2 核心不变量

1. 原始 Source 必须保留，Wiki 不能成为唯一事实来源。
2. 每个编译生成或修改的事实性 claim 必须关联至少一个 Source Chunk。
3. 新文档必须执行已有 Wiki 匹配，不能跳过 Match 和 Plan 直接写页面。
4. LLM 不能直接执行数据库写入或文件写入。
5. Compilation Plan 必须在 Apply 前通过确定性校验。
6. Apply 必须是原子操作；失败时不得留下部分 Wiki 更新。
7. Wiki 是默认检索对象，Source Chunk 是补充证据层。
8. Vector Search 必须参与默认检索，Wiki Link Expansion 不能被省略。
9. Compilation 和 Retrieval 的中间状态必须可以查看和调试。

## 5. 推荐技术栈

### 5.1 Backend

- Go 1.23+
- `net/http` + `github.com/go-chi/chi/v5`
- `github.com/danielgtaylor/huma/v2`，用于类型化 API contract、请求校验和 OpenAPI
- `database/sql` + `sqlc`
- `github.com/pressly/goose/v3` 管理数据库 migration
- `modernc.org/sqlite`，默认避免 CGO 依赖
- SQLite FTS5
- `github.com/philippgille/chromem-go`，作为本地嵌入式向量索引
- 标准库 `net/http` 实现轻量 OpenAI-compatible Chat/Embedding client
- `github.com/invopop/jsonschema` 生成 LLM structured output schema
- `github.com/go-playground/validator/v10` 加显式领域校验
- `github.com/ledongthuc/pdf`，用于文本型 PDF 提取
- Go 标准库 `testing`、`httptest` 和 `testify`

### 5.2 Frontend

- React
- TypeScript
- Vite
- React Router
- TanStack Query
- Tailwind CSS
- Lucide React
- react-markdown
- Vitest
- Playwright

### 5.3 本地运行方式

MVP 使用单个 Go 后端进程和单个 Vite 前端开发服务器。生产演示构建时，前端静态文件通过 `go:embed` 打包并由同一个 Go binary 托管。

暂不引入：

- Redis
- Celery
- Kafka
- PostgreSQL
- 独立向量数据库服务
- 微服务
- Tauri 或 Electron

## 6. 存储设计

### 6.1 存储分层

```text
SQLite
  规范化应用状态、关系、版本、编译记录、聊天记录

data/wiki/**/*.md
  Compiled Wiki 的人类可读投影和导出格式

data/sources/original/**
  不可替代的原始文件

data/sources/parsed/**
  可重建的解析结果

data/indexes/**
  chromem-go 等可重建索引
```

SQLite 是运行时 canonical store。Markdown Wiki 是稳定、可读、可用 Obsidian 打开的投影，但不单独承担事务和关系完整性。

### 6.2 为什么不只使用 Markdown

纯 Markdown 很适合人工浏览，但不适合可靠表达：

- Compilation Run 和 Plan 状态；
- statement 级 provenance；
- 页面 revision；
- 原子更新；
- 检索分数和 trace；
- Chat session；
- claim 与多个 Source Chunk 的多对多关系。

因此，Markdown 保留可读性，SQLite 负责规范化状态。

## 7. Wiki 数据模型

### 7.1 SourceDocument

主要字段：

- `id`
- `original_name`
- `media_type`
- `sha256`
- `original_path`
- `parsed_path`
- `status`
- `parser_version`
- `created_at`

### 7.2 SourceChunk

Chunk 仅用于解析、证据定位和 Source Retrieval，不是 Wiki Page。

主要字段：

- `id`
- `document_id`
- `chunk_index`
- `text`
- `page_number`
- `heading_path`
- `char_start`
- `char_end`
- `content_hash`

Chunk ID 必须稳定。在相同解析器版本和相同源内容下重新解析时，应生成相同 ID。

### 7.3 WikiPage

主要字段：

- `id`
- `slug`
- `page_type`: `concept | entity | topic`
- `title`
- `summary`
- `status`: `active | merged | archived`
- `current_revision`
- `created_at`
- `updated_at`

MVP 不为每篇 Source 建立一个独立 Wiki 知识页面。单文档摘要属于 Source 视图，不应与跨来源 Compiled Wiki 混淆。

### 7.4 WikiSection

主要字段：

- `id`
- `page_id`
- `heading`
- `position`
- `summary`

Section 用于页面渲染、向量索引和上下文预算控制。

### 7.5 WikiClaim

WikiClaim 是可追溯的事实或知识陈述。

主要字段：

- `id`
- `page_id`
- `section_id`
- `text`
- `claim_type`: `fact | definition | argument | procedure | caveat`
- `status`: `active | disputed | superseded`
- `created_by_run_id`
- `updated_by_run_id`

### 7.6 ClaimEvidence

主要字段：

- `claim_id`
- `source_chunk_id`
- `relation`: `support | contradict | context`
- `note`

Citation 最终必须落到 Source Chunk，而不能只引用 Wiki Page。

### 7.7 WikiLink

主要字段：

- `source_page_id`
- `target_page_id`
- `relation`: `related_to | part_of | contrasts_with | depends_on | instance_of`
- `created_by_run_id`

### 7.8 WikiRevision

主要字段：

- `page_id`
- `revision_number`
- `compilation_run_id`
- `snapshot_json`
- `change_summary`
- `created_at`

### 7.9 CompilationRun 和 CompilationPlan

保存：

- Source Document；
- Analyze 输出；
- Wiki 匹配候选及分数；
- 原始 Compilation Plan；
- Validation 结果；
- Apply 结果；
- 页面 diff；
- LLM model、prompt version、token usage、耗时和错误。

### 7.10 Conversation 和 Message

Message 除正文外还应保存：

- 原始用户问题；
- standalone retrieval query；
- retrieval trace ID；
- 使用的 Wiki passage；
- 使用的 Source Chunk；
- 最终 citations；
- 模型和 token usage。

## 8. Knowledge Compilation 设计

### 8.1 Pipeline

```text
Parse
  ↓
Analyze Source
  ↓
Retrieve Existing Wiki Candidates
  ↓
Create Compilation Plan
  ↓
Validate Plan
  ↓
Apply in Transaction
  ↓
Render Markdown
  ↓
Update Indexes
  ↓
Post-Apply Validation
```

### 8.2 Analyze

LLM 将 Source Chunk 组织为结构化分析：

- 文档摘要；
- 关键实体；
- 关键概念；
- claims 和 supporting chunk IDs；
- 方法、论点、结论和限制；
- 可能的矛盾；
- 与已有 Wiki 的潜在关系。

Analyze 输出必须是可由 Go struct、JSON Schema 和领域 validator 校验的 JSON，不接收自由格式 FILE block。JSON 解码必须拒绝未知字段，并对 ID、枚举、长度和跨对象引用做显式校验。

### 8.3 Match Existing Wiki

候选匹配由代码先召回，再由 LLM 判断：

1. slug/title 精确匹配；
2. alias 匹配；
3. FTS 匹配；
4. Wiki Page embedding 相似度；
5. 已有链接邻居补充。

每个待编译主题只向 Planner 提供有限数量的候选页面，避免让模型在整个 Wiki 中自由猜测。

### 8.4 Compilation Plan

页面级 action：

- `CREATE`
- `UPDATE`
- `MERGE`
- `LINK`
- `NO_OP`

Claim 级 action：

- `ADD`
- `REVISE`
- `RETAIN`
- `MARK_DISPUTED`
- `SUPERSEDE`

示意结构：

```json
{
  "document_id": "doc_123",
  "page_actions": [
    {
      "action": "UPDATE",
      "target_page_id": "page_attention",
      "reason": "The source adds new evidence to an existing concept.",
      "claim_actions": [
        {
          "action": "ADD",
          "text": "...",
          "evidence_chunk_ids": ["chunk_10", "chunk_11"]
        }
      ],
      "link_actions": []
    }
  ]
}
```

### 8.5 Validate

代码必须验证：

- action enum 合法；
- `UPDATE/MERGE/LINK` 的目标页面存在；
- `CREATE` 不与已有 slug 或高相似页面冲突；
- 所有 evidence chunk 属于当前或已存在 Source；
- 新增和修改 claim 至少有一个 evidence；
- link 目标存在且不能非法自链接；
- 同一 Plan 中没有互相冲突的 action；
- 页面类型和 relation 类型合法；
- Plan 大小没有超过安全上限。

### 8.6 Apply

Apply 由代码在数据库事务中执行：

1. 锁定相关页面；
2. 创建 revision snapshot；
3. 执行页面、section、claim 和 link action；
4. 写入 provenance；
5. 标记 Compilation Run；
6. 提交事务；
7. 生成 Markdown 投影；
8. 更新受影响页面的 FTS 和向量索引。

如果索引更新失败，数据库状态仍然是完整的，相关页面标记为 `index_pending` 并允许重建索引。

### 8.7 矛盾处理

MVP 不让 LLM 强行选择唯一真相。

当多个 Source 冲突时：

- 保留不同 claim；
- 将状态设为 `disputed`；
- 使用 `contradict` evidence relation；
- 在 Wiki 页面中渲染“不同来源的观点/结果”；
- Query 回答时明确表达冲突和来源。

## 9. Markdown Wiki 投影

推荐目录：

```text
data/wiki/
├── index.md
├── concepts/
├── entities/
└── topics/
```

示例页面：

```markdown
---
id: page_attention
type: concept
title: Attention Mechanism
revision: 3
updated_at: 2026-08-17T12:00:00Z
---

# Attention Mechanism

Attention is ... [^claim-1]

It is related to [[transformer-architecture]].

## Limitations

Different sources report ... [^claim-2]

## Sources

[^claim-1]: Source A, page 3, chunk `chunk_10`
[^claim-2]: Source B, page 8, chunk `chunk_42`
```

Markdown 由 renderer 确定性生成，不要求 LLM 维护 frontmatter、脚注编号或 index。

## 10. 向量索引方案

### 10.1 向量库

MVP 使用 chromem-go：

- 本地嵌入式运行；
- 无需单独部署服务；
- 使用纯 Go 实现，不要求 Python sidecar 或 CGO；
- 支持持久化 collection 和 metadata；
- 索引可删除后重建；
- 使用精确 cosine similarity，适合 Demo 规模。

MVP 不追求 ANN。若后续数据规模证明精确检索成为瓶颈，再通过 `VectorStore` 接口替换为 Qdrant、Milvus 或其他 ANN 实现，不改变上层 Retrieval Pipeline。

### 10.2 索引对象

主索引：Wiki Passage。

每个 passage 对应 Wiki Page 的一个 section 或一组相邻 claims，包含：

- `passage_id`
- `page_id`
- `section_id`
- `title`
- `heading_path`
- `text`
- `page_type`
- `revision`
- `embedding`

补充索引：Source Chunk。

Source Chunk 只在 Wiki 证据不足、需要精确原文或用户明确要求 Source-only 时参与上下文。

### 10.3 Embedding 配置

配置至少包括：

- endpoint；
- API key；
- model；
- dimension；
- batch size。

每条向量记录保存 `embedding_model` 和 `content_hash`。模型或正文变化时必须重建对应向量。

## 11. Hybrid Retrieval Pipeline

默认流程：

```text
User Query
  ↓
Query Normalization
  ↓
Wiki FTS Search + Wiki Vector Search
  ↓
RRF Fusion
  ↓
Wiki Link Expansion
  ↓
Deterministic Rerank
  ↓
Source Evidence Fetch / Fallback
  ↓
Context Budgeting
  ↓
Answer Generation
```

### 11.1 初始召回

并行执行：

- FTS5 搜索 Wiki Passage；
- chromem-go 搜索 Wiki Passage。

建议默认：

- FTS Top 20；
- Vector Top 20；
- RRF 后保留 Top 10 个 Wiki seeds。

### 11.2 Fusion

MVP 使用 Reciprocal Rank Fusion：

```text
score = Σ 1 / (k + rank_i)
```

建议 `k=60`。避免直接混合不可比较的 BM25 和 cosine 原始分数。

### 11.3 Wiki Link Expansion

从 Top Wiki seeds 做一跳扩展：

- 优先显式 WikiLink；
- relation 可配置权重；
- 避免无限遍历；
- 扩展结果最多占最终 Wiki 上下文的 20%–30%。

MVP 不做两跳扩展和图算法社区分析。

### 11.4 Rerank

MVP 使用可解释的确定性分数：

- RRF rank；
- title exact match；
- page type；
- 是否是 graph-expanded result；
- 与多个 seed 的连接数；
- provenance coverage；
- freshness 或 revision 状态。

第一版不引入额外 cross-encoder。后续只有在评估证明有必要时再增加 LLM/cross-encoder reranker。

### 11.5 Source Retrieval

以下情况触发 Source Chunk Retrieval：

- Wiki Passage 的 provenance 不足；
- 问题要求具体数字、引文、页码或原话；
- Wiki 召回低于阈值；
- 用户选择 Source-only；
- Wiki 中存在 disputed claims。

Source Retrieval 的结果用于验证和补充 Wiki，不替代 Wiki 作为主要知识表示。

### 11.6 Retrieval Trace

每次检索保存：

- normalized query；
- FTS candidates；
- vector candidates；
- RRF score；
- graph expansion 来源；
- rerank score；
- 被选择或丢弃的原因；
- context token budget；
- 最终 Wiki Passage 和 Source Chunk。

## 12. Query 与 Chat 架构

### 12.1 Query

Query 是单轮调用：

```text
Question -> Retrieve -> Build Context -> Generate -> Cite
```

返回：

- answer；
- citations；
- referenced Wiki Pages；
- retrieval trace ID；
- latency 和 token usage。

### 12.2 Chat

Chat 使用同一个 Retriever，不另建 Agent 检索路径。

每轮流程：

1. 读取有限的会话历史；
2. 将追问改写为 standalone retrieval query；
3. 使用 standalone query 检索；
4. 上下文中保留用户原始问题；
5. 生成流式回答；
6. 保存检索快照、引用和消息。

### 12.3 Context Budget

建议按模型上下文动态分配：

- System 和 citation rules：10%；
- Chat history：20%；
- Wiki Passage：45%；
- Source Evidence：20%；
- 输出预留：5% 或由模型配置单独控制。

优先放入 Wiki Passage，再补其直接证据。禁止为了放入更多文本而绕过 Wiki，直接塞入大量 Source Chunk。

### 12.4 Citation

模型上下文中的证据使用稳定编号，例如：

```text
[W1] Wiki passage ...
  Evidence: [S1], [S2]

[S1] Source document A, page 3, chunk_10 ...
```

最终答案 citation 必须解析并校验：

- 引用 ID 必须存在于本轮 context；
- UI 最终链接到 Source Chunk；
- 可以同时展示所属 Wiki Page；
- 无法找到证据时明确回答“现有知识库中没有足够证据”。

## 13. API 草案

### Source

```text
POST   /api/sources
GET    /api/sources
GET    /api/sources/{id}
GET    /api/sources/{id}/chunks
POST   /api/sources/{id}/compile
```

### Compilation

```text
GET    /api/compilations
GET    /api/compilations/{id}
GET    /api/compilations/{id}/plan
GET    /api/compilations/{id}/diff
```

MVP 默认自动 Apply 已通过校验的 Plan。API 仍保留 Plan 和 diff 查询能力，便于观察。

### Wiki

```text
GET    /api/wiki/pages
GET    /api/wiki/pages/{id-or-slug}
GET    /api/wiki/pages/{id}/links
GET    /api/wiki/pages/{id}/revisions
GET    /api/wiki/pages/{id}/sources
POST   /api/wiki/reindex
```

### Retrieval

```text
POST   /api/search
GET    /api/retrieval-traces/{id}
```

### Query / Chat

```text
POST   /api/query
POST   /api/conversations
GET    /api/conversations
GET    /api/conversations/{id}
POST   /api/conversations/{id}/messages
DELETE /api/conversations/{id}
```

Chat 和长任务进度使用 SSE，不在 MVP 引入 WebSocket。

## 14. Repository 目录结构

```text
llm_wiki_demo/
├── DESIGN_PRINCIPLES.md
├── DEVELOPMENT_PLAN.md
├── README.md
├── Makefile
├── .env.example
├── docker-compose.yml
├── backend/
│   ├── go.mod
│   ├── go.sum
│   ├── sqlc.yaml
│   ├── cmd/
│   │   └── server/
│   │       └── main.go
│   ├── internal/
│   │   ├── api/
│   │   ├── config/
│   │   ├── db/
│   │   │   ├── migrations/
│   │   │   ├── queries/
│   │   │   └── generated/
│   │   ├── domain/
│   │   ├── sources/
│   │   ├── parsers/
│   │   ├── compiler/
│   │   ├── wiki/
│   │   ├── indexing/
│   │   ├── retrieval/
│   │   ├── query/
│   │   ├── chat/
│   │   └── llm/
│   ├── prompts/
│   └── web/
│       └── dist/
├── frontend/
│   ├── package.json
│   └── src/
│       ├── api/
│       ├── components/
│       ├── features/
│       │   ├── sources/
│       │   ├── compilation/
│       │   ├── wiki/
│       │   ├── search/
│       │   └── chat/
│       ├── pages/
│       └── routes/
├── tests/
│   ├── unit/
│   ├── integration/
│   ├── e2e/
│   └── fixtures/
├── eval/
│   ├── dataset/
│   ├── runner/
│   └── reports/
├── scripts/
└── data/
    ├── sources/
    │   ├── original/
    │   └── parsed/
    ├── wiki/
    └── indexes/
```

## 15. 分阶段开发计划

## Phase 0：工程骨架与核心契约

### 目标

建立可运行、可测试的全栈工程边界，并先固定数据契约。

### 主要实现内容

- 初始化 Go module、chi router、Huma、`database/sql`、sqlc 和 goose。
- 初始化 React、TypeScript、Vite 和路由。
- 实现统一配置、日志、错误响应和 request ID。
- 定义 Source、Wiki、Compilation Plan、Retrieval Trace 和 Chat 的 Go 领域类型与 API DTO。
- 定义 SQL migration 和 sqlc query，禁止业务层散落手写 SQL。
- 建立 JSON Schema 生成与严格 JSON decode/validation 工具。
- 提供 health/config status API。
- 提供 `.env.example`、Makefile 和基础 README。

### 涉及模块或目录

- `backend/cmd/server/main.go`
- `backend/internal/api/`
- `backend/internal/config/`
- `backend/internal/db/`
- `backend/internal/domain/`
- `backend/internal/llm/`
- `backend/sqlc.yaml`
- `frontend/src/`
- `tests/unit/`

### 前置依赖

无。

### 验收标准

- 一条命令可启动后端和前端。
- `/api/health` 返回数据库和应用状态。
- 未配置 LLM/Embedding API 时，系统可启动并明确显示缺失配置。
- `go test ./...`、`go vet ./...` 和 `go build ./cmd/server` 通过。
- Frontend lint、typecheck 和 unit test 通过。
- OpenAPI 中可看到核心 schema。

## Phase 1：Source 保存、解析与证据定位

### 目标

可靠保留 Ground Truth，并生成稳定、可定位的 Source Chunk。

### 主要实现内容

- 上传和保存 Markdown、TXT、文本型 PDF。
- 计算 SHA256，支持幂等去重。
- Markdown/TXT 按标题和段落解析。
- PDF 按页解析并保存页码。
- 按 heading、段落和 token 上限切分 Chunk。
- 保存字符区间、标题路径、页码和 content hash。
- 实现 Source 列表、详情和 Chunk 浏览 API。
- 保存解析错误和状态，不留下半完成记录。

### 涉及模块或目录

- `backend/internal/sources/`
- `backend/internal/parsers/`
- `backend/internal/domain/source.go`
- `backend/internal/api/sources.go`
- `backend/internal/db/queries/sources.sql`
- `data/sources/`
- `tests/unit/parsers/`
- `tests/integration/test_source_ingest.py`

### 前置依赖

Phase 0。

### 验收标准

- Markdown、TXT、文本型 PDF 均可导入。
- 重复文件返回 `NO_OP`，不创建重复 Source。
- 每个 Chunk 可以定位回文件、页码或标题路径。
- 相同文件重新解析时 Chunk ID 保持稳定。
- 解析失败时原始文件仍保留，数据库状态为 `failed`。
- Source 尚未编译时，不会出现在 Wiki 中。

## Phase 2：Knowledge Compiler

### 目标

实现项目最关键的 Knowledge Compilation 流程：

```text
Parse -> Analyze -> Match -> Plan -> Validate -> Apply
```

### 主要实现内容

- 实现结构化 Analyze prompt、Go DTO 和 JSON Schema。
- 从 Source 提取实体、概念、claims、关系和矛盾。
- 实现已有 Wiki 候选匹配。
- 实现结构化 Compilation Plan。
- 支持 `CREATE/UPDATE/MERGE/LINK/NO_OP`。
- 实现 claim-level action 和 evidence 校验。
- 实现事务 Applier 和 Wiki Revision。
- 实现 Markdown renderer。
- 保存 Compilation Run、Plan、validation 和 diff。
- 定义 `LLMClient` 和 `EmbeddingClient` interface，并提供 deterministic fake adapter。

### 涉及模块或目录

- `backend/internal/compiler/analyzer.go`
- `backend/internal/compiler/matcher.go`
- `backend/internal/compiler/planner.go`
- `backend/internal/compiler/validator.go`
- `backend/internal/compiler/applier.go`
- `backend/internal/compiler/renderer.go`
- `backend/internal/domain/wiki.go`
- `backend/internal/domain/compilation.go`
- `backend/internal/llm/`
- `backend/prompts/`
- `backend/internal/db/queries/wiki.sql`
- `backend/internal/db/queries/compilations.sql`
- `tests/unit/compiler/`
- `tests/integration/test_compilation.py`

### 前置依赖

- Phase 1。
- 可用的 OpenAI-compatible Chat Completion 配置。
- 可用的 Embedding 配置，或测试环境中的 deterministic embedding adapter。

### 验收标准

- 第一篇文档能够创建 Concept/Entity/Topic 页面。
- 第二篇相关文档优先更新已有页面，而不是生成近重复页面。
- 同一页面能够累积来自多个 Source 的 claims。
- 每个新增或修改的 claim 都有有效 Source Chunk evidence。
- 非法 page ID、chunk ID 或 action 无法 Apply。
- Apply 中途失败时，Wiki 和 revision 均不发生部分更新。
- `NO_OP` 不生成无意义 revision。
- 用户可以通过 API 查看 Analyze、候选、Plan、diff 和 Apply 结果。

## Phase 3：Wiki 服务与结构浏览

### 目标

让 Compiled Wiki 成为系统和 UI 的中心知识表示。

### 主要实现内容

- Wiki 页面列表和类型过滤。
- 页面详情、section、claim 和 evidence。
- Wiki links、backlinks 和 related pages。
- Source 到 Wiki 的反向追踪。
- Revision history 和页面 diff。
- 确定性生成 `index.md` 和页面 Markdown。
- 实现结构 lint。
- 前端实现 Wiki tree、reader 和 citation/source inspector。

### 涉及模块或目录

- `backend/internal/wiki/`
- `backend/internal/api/wiki.go`
- `data/wiki/`
- `frontend/src/features/wiki/`
- `frontend/src/features/sources/`
- `tests/unit/wiki/`
- `tests/integration/test_wiki_projection.py`

### 前置依赖

Phase 2。

### 验收标准

- 页面展示跨来源综合知识，而非单文档摘要。
- 页面可以查看所有来源和 claim-level citation。
- 点击 citation 可以打开对应 Source Chunk。
- 页面之间的 wikilink 和 backlink 一致。
- Markdown 重复生成时结果幂等。
- Lint 能检测断链、重复 slug、无证据 claim 和孤立 evidence。
- 页面 revision 可以展示每次编译带来的变化。

## Phase 4：向量索引与 Hybrid Retrieval

### 目标

实现以下默认检索链路：

```text
Query
-> Wiki FTS + Wiki Vector Search
-> RRF
-> Wiki Link Expansion
-> Rerank
-> Source Evidence
-> Context
```

### 主要实现内容

- 为 Wiki Passage 建立 FTS5 索引。
- 为 Wiki Passage 建立 chromem-go 向量索引。
- 为 Source Chunk 建立补充向量索引。
- 实现 embedding batch、content hash 和增量更新。
- 实现 RRF。
- 实现一跳 Wiki Link Expansion。
- 实现确定性 rerank 和 context selection。
- 实现 Source fallback。
- 保存 Retrieval Trace。
- 实现完整 reindex 命令/API。

### 涉及模块或目录

- `backend/internal/indexing/`
- `backend/internal/retrieval/fts.go`
- `backend/internal/retrieval/vector.go`
- `backend/internal/retrieval/fusion.go`
- `backend/internal/retrieval/expansion.go`
- `backend/internal/retrieval/reranker.go`
- `backend/internal/retrieval/context.go`
- `backend/internal/api/search.go`
- `data/indexes/`
- `tests/unit/retrieval/`
- `tests/integration/test_reindex.py`

### 前置依赖

- Phase 2。
- Phase 3。
- Embedding provider 或 deterministic test embedding。

### 验收标准

- 关键词查询和语义改写查询均能召回目标 Wiki Page。
- 默认请求会同时运行 FTS 和 Vector Search。
- Wiki links 能补充初始向量结果未直接召回的相关页面。
- 每个结果都包含 FTS、vector、RRF、graph 和 final score。
- Trace 能说明候选为何被选择或丢弃。
- Source 默认只作为补充证据，不替代 Wiki 主检索。
- 删除 `data/indexes` 后可以从 SQLite 完整重建。

## Phase 5：Query 与多轮 Chat

### 目标

基于同一个 Hybrid Retrieval Pipeline 提供单轮 Query 和多轮 Chat。

### 主要实现内容

- 实现 Query service 和 API。
- 实现 Conversation/Message 持久化。
- 实现多轮问题的 standalone query rewrite。
- 实现上下文预算和去重。
- 实现 citation-aware answer prompt。
- 实现 SSE 流式输出。
- 解析并校验模型输出的 citation。
- 保存每轮 Retrieval Trace 和引用快照。
- 实现会话创建、列表、恢复和删除。

### 涉及模块或目录

- `backend/internal/query/`
- `backend/internal/chat/`
- `backend/internal/api/query.go`
- `backend/internal/api/chat.go`
- `frontend/src/features/chat/`
- `tests/unit/query/`
- `tests/integration/test_chat.py`

### 前置依赖

Phase 4。

### 验收标准

- Query 和 Chat 调用完全相同的 Retriever。
- 追问能够利用会话历史解析指代。
- 每条事实性回答带有可点击 Source citation。
- Citation ID 必须存在于本轮 context，伪造 citation 会被拒绝或移除。
- 证据不足时明确说明，不能生成虚假来源。
- 会话在进程重启后仍可恢复。
- 每轮消息可查看引用的 Wiki Page、Source Chunk 和 Retrieval Trace。

## Phase 6：完整 Demo UI 与可观察性

### 目标

提供一个可以清楚演示 Knowledge Compilation、Wiki、Retrieval 和 Chat 的完整 Web UI。

### 主要实现内容

- 左侧主导航：Wiki、Sources、Compilation、Search、Chat、Settings。
- Source 上传和解析状态。
- Compilation 进度、Plan action 和页面 diff。
- Wiki 页面阅读、wikilink、backlink 和 revision。
- Citation inspector，展示 Source 原文位置。
- Search 页展示 FTS、Vector、Expansion 和 Rerank trace。
- Chat 页面展示流式回答和引用。
- Settings 配置 Compiler LLM、Chat LLM 和 Embedding model。
- 完整 loading、empty、error 和 retry 状态。

### 涉及模块或目录

- `frontend/src/routes/`
- `frontend/src/pages/`
- `frontend/src/features/sources/`
- `frontend/src/features/compilation/`
- `frontend/src/features/wiki/`
- `frontend/src/features/search/`
- `frontend/src/features/chat/`
- `frontend/src/api/`
- `tests/e2e/`

### 前置依赖

Phase 1 至 Phase 5。

### 验收标准

- 用户可以完成：上传第一篇文档 -> 查看 CREATE Plan -> 查看 Wiki。
- 用户可以继续上传相关文档 -> 查看 UPDATE Plan -> 查看融合后的同一页面。
- 用户可以提问并打开最终 citation 对应的 Source 原文。
- 用户可以查看一次检索的各阶段候选和分数。
- 桌面和移动视口无重叠或不可访问控件。
- 核心演示流程有 Playwright E2E 测试。
- UI 不把 Wiki 降级为上传页面旁边的装饰性视图。

## Phase 7：测试、Evaluation 与交付

### 目标

证明系统满足设计原则，并可在新环境中稳定运行和演示。

### 主要实现内容

- 补齐 parser、validator、applier、renderer、RRF 和 graph expansion 单元测试。
- 增加事务回滚、索引重建和 Chat 集成测试。
- 构建 8–12 篇相关小语料，包含重复主题、补充事实和冲突来源。
- 编写 gold Compilation expectations。
- 编写 gold retrieval queries 和 expected pages/chunks。
- 实现离线 evaluation runner。
- 输出指标报告。
- 提供 Docker Compose 或统一启动命令。
- 编写演示脚本和故障排查文档。

### 涉及模块或目录

- `tests/unit/`
- `tests/integration/`
- `tests/e2e/`
- `tests/fixtures/`
- `eval/dataset/`
- `eval/runner/`
- `eval/reports/`
- `scripts/`
- `README.md`

### 前置依赖

Phase 0 至 Phase 6。

### 验收标准

- 所有自动化测试通过。
- Wiki active claim provenance coverage 为 100%。
- 第二来源应更新已有页面的测试用例全部通过。
- 重复页面率低于评测集定义的阈值。
- Hybrid Retrieval 的 Recall@5 高于纯 FTS 和纯 Vector 基线。
- Citation precision 达到评测集约定阈值。
- Answer groundedness 通过规则校验和抽样 LLM judge。
- 全新环境可以根据 README 在十分钟内完成启动和示例导入。

## 16. 测试策略

### 16.1 Unit Tests

重点覆盖：

- Source hash 和稳定 Chunk ID；
- Markdown/PDF parsing；
- Analyze/Plan schema validation；
- Compilation action validation；
- claim evidence invariants；
- transaction Apply；
- Markdown renderer；
- FTS query；
- RRF；
- graph expansion；
- context budget；
- citation parser。

### 16.2 Integration Tests

重点场景：

1. 导入第一篇文档并创建 Wiki。
2. 导入第二篇相关文档并更新已有页面。
3. 导入语义相近但主体不同的文档，不错误合并。
4. 导入冲突 Source，保留 disputed claims。
5. Apply 失败后完整回滚。
6. 删除并重建所有索引。
7. Query 返回 Source citation。
8. Chat follow-up 使用历史但重新检索。

### 16.3 Contract Tests

所有 LLM 输出必须通过 schema contract test。测试中使用固定响应 fixture，不依赖真实 LLM。

真实 LLM 测试单独标记为 `real_llm`，默认 CI 不运行。

### 16.4 E2E Tests

至少覆盖：

- 上传和编译；
- Plan/diff 查看；
- Wiki 浏览；
- Citation 跳转；
- Search trace；
- Query；
- 多轮 Chat；
- 错误和重试。

## 17. Evaluation 方案

### 17.1 Compilation Evaluation

指标：

- `Page Match Accuracy`：是否选择了正确已有页面。
- `Action Accuracy`：CREATE/UPDATE/MERGE/LINK/NO_OP 是否正确。
- `Duplicate Page Rate`：应融合却创建新页面的比例。
- `Claim Preservation`：更新后是否保留旧来源支持的有效 claim。
- `Provenance Coverage`：active claim 中有 evidence 的比例。
- `Evidence Validity`：claim 引用的 chunk 是否真实支持该 claim。
- `Subject Boundary Error`：是否将相似术语下的不同主体错误融合。

### 17.2 Retrieval Evaluation

对以下三组基线分别测量：

1. FTS only；
2. Vector only；
3. Hybrid + Wiki expansion。

指标：

- Recall@5；
- Recall@10；
- MRR；
- Wiki Page Recall；
- Evidence Chunk Recall；
- Expansion Contribution；
- 无关 graph expansion 比例。

### 17.3 Answer Evaluation

指标：

- Citation Precision；
- Citation Completeness；
- Groundedness；
- Conflict Awareness；
- Unsupported Claim Rate；
- 多轮上下文正确率。

### 17.4 可重复性

Evaluation 数据、expected page IDs、expected actions 和 queries 必须纳入仓库。所有指标应能通过单条命令重新计算并生成 JSON/Markdown 报告。

## 18. MVP 范围

### 18.1 MVP 必须实现

- Markdown、TXT、文本型 PDF 导入。
- 原始 Source 保留和 SHA256 去重。
- 稳定 Source Chunk 和原文定位。
- Analyze -> Match -> Plan -> Validate -> Apply。
- `CREATE/UPDATE/MERGE/LINK/NO_OP`。
- 新文档与已有 Wiki 融合。
- Concept、Entity、Topic Wiki Page。
- Claim-level Citation 和 Provenance。
- Wiki Markdown 投影。
- Wiki FTS5 索引。
- Wiki 和 Source 的 chromem-go 向量索引。
- RRF Hybrid Retrieval。
- 一跳 Wiki Link Expansion。
- Source evidence fallback。
- Query。
- 持久化多轮 Chat。
- SSE 流式回答。
- Compilation Plan/diff 可视化。
- Retrieval Trace 可视化。
- Wiki、Source 和 Citation 浏览。
- 基础测试和一套可重复演示数据。

### 18.2 MVP 暂不实现

- OCR 和扫描 PDF。
- 图片、图表和音视频理解。
- Office、EPUB 和网页抓取。
- 文件夹自动监听。
- 多用户、认证和权限系统。
- 云部署和分布式任务队列。
- 人工审批工作流。
- 自动 Web Research。
- 通用工具调用 Agent。
- Skill、Deck 和报告生成器。
- Graph community detection。
- 复杂 ontology editor。
- Cross-encoder 或 LLM reranker。
- 自动删除 Source 后的知识撤销。
- 实时协同编辑。

## 19. 最大技术风险

### 19.1 增量融合正确性

这是项目最大的风险，比向量数据库或前端复杂度更重要。

可能的问题：

- 错误地把不同概念合并到同一页；
- 应更新已有页时创建重复页；
- 新来源覆盖旧来源中的有效事实；
- 将不同主体共享的术语错误泛化；
- 将冲突来源强行合并成一个结论；
- 生成没有原始证据支持的综合 claim。

控制措施：

- 代码先召回有限候选；
- Planner 只能从候选中选择 UPDATE/MERGE 目标；
- 使用稳定 page/claim ID；
- 强制 claim evidence；
- LLM 不能直接写库；
- Apply 前严格校验；
- 事务更新和 revision snapshot；
- 冲突 claim 并存；
- 为第二来源融合和主体边界建立专门评测集。

### 19.2 Citation 粒度

只在页面 frontmatter 保存 `sources` 不能证明具体 claim 来自哪里。因此 MVP 必须实现 claim 到 Source Chunk 的映射，不能把 statement-level provenance 推迟到以后。

### 19.3 LLM 输出稳定性

控制措施：

- 使用 Go struct 生成的 JSON Schema 和 provider structured output；
- 严格 JSON decoder 拒绝未知字段和尾随内容；
- schema version；
- bounded retries；
- 原始响应留档；
- fake LLM adapter；
- 不解析自由格式 FILE block；
- 不自动修复无法验证的关键 ID。

### 19.4 索引一致性

索引必须被视为派生状态。SQLite commit 成功但索引更新失败时，不回滚 Wiki，而是记录 `index_pending`，由 retry/reindex 修复。

## 20. 推荐实际开发顺序

严格按以下顺序执行：

```text
Phase 0  工程骨架和 schema
    ↓
Phase 1  Source 保存和解析
    ↓
Phase 2  Knowledge Compiler
    ↓
Phase 3  Wiki 服务和可追溯浏览
    ↓
Phase 4  Hybrid Retrieval
    ↓
Phase 5  Query / Chat
    ↓
Phase 6  完整 Demo UI
    ↓
Phase 7  Evaluation 和交付
```

Phase 2 完成后，必须先通过一个最小垂直验证：

1. 导入文档 A；
2. 创建一个 Concept Page；
3. 导入相关文档 B；
4. 生成 `UPDATE` 而不是重复 `CREATE`；
5. 同一 Wiki Page 出现来自 A 和 B 的 claims；
6. 每个 claim 可以跳转到对应 Source Chunk；
7. Compilation Plan 和 diff 可以完整查看。

该验证通过后再继续开发 Retrieval 和 Chat。否则项目很容易先完成传统 RAG，再把 Wiki 变成非核心的展示层。

## 21. Coding Agent 执行规则

每个 Phase 应独立提交和验收。执行某个 Phase 时，Coding Agent 应：

1. 先阅读 `DESIGN_PRINCIPLES.md` 和本文档。
2. 只实现当前 Phase 及其必要依赖。
3. 不提前加入“暂不实现”能力。
4. 为新增核心不变量添加测试。
5. 保持 LLM 语义判断与代码确定性执行的边界。
6. 不允许绕过 Compiled Wiki，直接将 Source Chunk 作为默认问答上下文。
7. 在 Phase 结束时运行测试并逐项报告验收标准。
8. 若实现与本文档发生冲突，以 `DESIGN_PRINCIPLES.md` 为最高优先级。

## 22. MVP 完成定义

当且仅当以下端到端流程可以稳定演示时，MVP 才算完成：

```text
上传 Source A
  ↓
查看 Analyze 和 CREATE Plan
  ↓
生成带 Source Citation 的 Wiki Page
  ↓
上传相关 Source B
  ↓
查看 Match Candidate 和 UPDATE Plan
  ↓
查看融合后的同一 Wiki Page 及两个来源
  ↓
输入 Query
  ↓
查看 Vector + FTS + Link Expansion Trace
  ↓
获得引用原始 Source 的回答
  ↓
继续追问并保持多轮上下文
```

这个流程必须清楚证明：知识先被编译进 Wiki，再通过向量搜索和 Wiki 结构被检索，而不是在每次问答时从原始 Chunk 中重新临时发现全部知识。
