# LLM-Wiki Demo — Design Principles

## 1. Knowledge Is Compiled, Not Just Indexed

核心链路必须是：

```text
Document → Understand → Compile → Wiki → Retrieve
```

而不是传统 RAG 的：

```text
Document → Chunk → Embedding → Retrieve
```

Wiki 必须是经过 LLM 理解、重组后的知识表示。

## 2. Raw Sources Are Ground Truth

原始文档必须保留。

Wiki 是对 Source 的知识编译结果，不是新的唯一事实来源。Wiki 内容必须尽可能能够追溯到原始 Source。

## 3. Wiki Pages Are Knowledge Units, Not Chunks

Wiki Page 应表示 Concept、Entity、Topic 或综合知识，而不是 `chunk_001`、`chunk_002`。

Chunk 只用于解析、引用和 Source Retrieval。

## 4. New Knowledge Must Integrate With Existing Knowledge

新增文档时，必须先检查已有 Wiki，再决定：

```text
CREATE / UPDATE / MERGE / LINK / NO-OP
```

优先更新和融合已有知识，避免重复页面。

## 5. Compilation Must Be Plan-Then-Write

编译流程应保持：

```text
Parse
→ Analyze
→ Match Existing Wiki
→ Compilation Plan
→ Apply
→ Validate
```

LLM 负责语义判断，代码负责确定性执行。

## 6. Knowledge Must Accumulate Across Sources

Document Summary 和 Wiki Knowledge 必须区分。

Summary 回答：

> 这篇文档说了什么？

Wiki 回答：

> 综合所有来源，我们现在知道什么？

同一 Wiki Page 应能持续吸收多个 Source 的知识。

## 7. Wiki and Vector Search Are Complementary

Wiki 是 **Knowledge Representation**。

Vector Search 是 **Retrieval Mechanism**。

主要对编译后的 Wiki 建立向量索引，同时保留 Source Chunk Retrieval 作为补充证据层。

## 8. Retrieval Must Combine Vector Search and Wiki Structure

默认检索流程：

```text
Query
→ Vector Search
→ Wiki Link Expansion
→ Rerank
→ Context
→ Answer
```

Vector Search 用于找入口，Wiki Link 用于补充结构关系。

必要时再回退到原始 Source。

## 9. Compilation and Retrieval Must Be Observable and Decoupled

必须能够观察：

```text
Document → Compilation Plan → Wiki Changes
```

以及：

```text
Query → Candidates → Expansion → Rerank → Context
```

同时保持：

```text
Compiler → Wiki → Indexer → Retrieval
```

各模块可以独立替换。

## 10. The System Must Not Degrade Into Traditional RAG

如果最终实现只是：

```text
Upload → Chunk → Vector DB → LLM
```

再额外生成几个 Wiki 页面用于展示，那么项目方向就是错误的。

正确架构必须始终保持：

```text
Raw Sources
    ↓
Knowledge Compiler
    ↓
Compiled Wiki
    ↓
Vector Search + Wiki Structure
    ↓
Query / Chat
```

**Compiled Wiki 必须处于系统中心。**

