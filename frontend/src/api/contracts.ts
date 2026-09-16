import { getSelectedKnowledgeBaseID } from './scope'

export type ProviderStatus = { configured: boolean; endpoint_configured: boolean; model?: string }
export type HealthResponse = { status: string; database: string; providers: { compiler_llm: ProviderStatus; chat_llm: ProviderStatus; embedding: ProviderStatus } }
export type LLMSettings = { base_url: string; model: string; api_key_set: boolean; fake_fallback?: boolean }
export type EmbeddingSettings = { base_url: string; model: string; batch_size?: number }
export type Settings = { llm: LLMSettings; embedding: EmbeddingSettings; language: 'zh' | 'en' }
export type ConfigStatus = { providers: HealthResponse['providers']; settings: Settings }
export type SettingsInput = { llm: { base_url: string; model: string; api_key: string; fake_fallback: boolean }; embedding: { base_url: string; model: string; batch_size: number }; language: 'zh' | 'en' }
export type KnowledgeBase = { id: string; name: string; description: string; status: 'active' | 'archived'; language: 'zh' | 'en'; llm_base_url?: string; llm_model?: string; llm_api_key_set: boolean; embedding_base_url?: string; embedding_model?: string; created_at: string; updated_at: string }
export type KnowledgeBaseInput = { name: string; description: string; language: 'zh' | 'en' }
export type PageType = 'concept' | 'entity' | 'topic'
export type WikiPage = { id: string; slug: string; page_type: PageType; title: string; summary: string; status: string; current_revision: number; updated_at: string }
export type WikiPageListItem = WikiPage & { claim_count: number; source_count: number; link_count: number }
export type SourceDocument = { id: string; original_name: string; media_type: string; status: string; created_at: string }
export type SourceChunk = { id: string; document_id: string; chunk_index: number; text: string; page_number?: number; heading_path?: string[]; char_start: number; char_end: number }
export type EvidenceView = { evidence: { claim_id: string; source_chunk_id: string; relation: string; note?: string }; chunk: SourceChunk; source: SourceDocument }
export type ClaimView = { claim: { id: string; text: string; claim_type: string; status: string }; evidence: EvidenceView[] }
export type SectionView = { section: { id: string; heading: string; summary: string }; claims: ClaimView[] }
export type LinkView = { link: { source_page_id: string; target_page_id: string; relation: string }; page: WikiPage }
export type SourceTrace = { source: SourceDocument; chunk_count: number; claim_count: number; evidence: EvidenceView[] }
export type WikiPageDetail = { page: WikiPage; canonical_page?: WikiPage; sections: SectionView[]; claims: ClaimView[]; links: LinkView[]; backlinks: LinkView[]; related: LinkView[]; sources: SourceTrace[] }
export type GraphNode = { id: string; slug: string; title: string; page_type: PageType; summary: string; claim_count: number; source_count: number; link_count: number }
export type GraphEdge = { source: string; target: string; relation: string }
export type WikiGraph = { nodes: GraphNode[]; edges: GraphEdge[] }
export type RevisionDiff = { added_claims: string[]; removed_claims: string[]; changed: string[] }
export type RevisionView = { revision: { page_id: string; revision_number: number; compilation_run_id: string; change_summary: string; created_at: string }; diff: RevisionDiff }
export type RetrievalCandidate = { passage_id: string; page_id: string; slug: string; title: string; page_type: string; text: string; fts_score: number; vector_score: number; rrf_score: number; expansion_score: number; final_score: number; expanded: boolean; expansion_from?: string[]; selected: boolean; discard_reason?: string }
export type RetrievalTrace = { id: string; normalized_query: string; fts_candidates: string[]; vector_candidates: string[]; rrf_score: Record<string, number>; expanded_pages: string[]; final_candidates?: string[]; dropped_candidates?: string[]; context_ids?: string[]; context_budget: number; context_used: number; notes?: string[] }
export type RetrievalContext = { id: string; kind: string; page_id?: string; passage_id?: string; source_chunk_id?: string; text: string; citation: string }
export type RetrievalResult = { query: string; candidates: RetrievalCandidate[]; context: RetrievalContext[]; trace: RetrievalTrace }
export type CitationSnapshot = { id: string; kind: string; page_id?: string; passage_id?: string; source_chunk_id?: string; label: string; text: string }
export type QueryResult = { question: string; answer: string; citations: CitationSnapshot[]; wiki_references: CitationSnapshot[]; context: CitationSnapshot[]; retrieval_trace: RetrievalTrace }
export type Conversation = { id: string; title: string; created_at: string; updated_at: string }
export type Message = { id: string; conversation_id: string; role: string; content: string; retrieval_trace_id?: string; standalone_query?: string; citations?: CitationSnapshot[]; context?: CitationSnapshot[]; created_at: string }
export type ConversationDetail = { conversation: Conversation; messages: Message[] }
export type ChatTurnResult = { user_message: Message; assistant_message: Message; result: QueryResult }
export type SourceUpload = { source: SourceDocument; chunks: SourceChunk[]; result: { action: 'CREATED' | 'NO_OP' } }
export type SourceChunkDetail = { source: SourceDocument; chunk: SourceChunk }
export type SourceWikiTrace = { source: SourceDocument; chunk_count: number; claim_count: number; evidence: EvidenceView[] }
export type CompilationPlan = { document_id: string; page_actions: Array<{ action: string; target_page_id?: string; source_page_id?: string; slug?: string; page_type?: string; title?: string; reason: string; claim_actions?: Array<{ action: string; text: string; evidence_chunk_ids: string[] }> }> }
export type CompilationRun = { id: string; document_id: string; status: string; analyze?: { summary?: string; topics?: Array<{ key: string; title: string; page_type: string; claims: unknown[] }> }; candidates?: Array<{ topic_key: string; page_id: string; title: string; score: number }>; plan?: CompilationPlan; validation?: { valid: boolean; error?: string }; apply_result?: Record<string, unknown>; diff?: { pages?: Array<Record<string, unknown>>; added_claims?: string[]; changed_claims?: string[]; removed_claims?: string[] } | Record<string, unknown>; model?: string; prompt_version?: string; error?: string; created_at: string }

type ErrorDetail = { code?: string; message?: string; request_id?: string }
type ErrorPayload = { error?: ErrorDetail | string }

function errorMessage(payload: ErrorPayload | null, fallback: string): string {
  const error = payload?.error
  const message = typeof error === 'string' ? error : error?.message
  const requestID = typeof error === 'object' ? error?.request_id : undefined
  return `${message || fallback}${requestID ? ` (request_id: ${requestID})` : ''}`
}

function sseErrorPayload(raw: string): ErrorPayload | null {
  const data = raw.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n')
  if (!data) return null
  try {
    return JSON.parse(data) as ErrorPayload
  } catch {
    return null
  }
}

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path, { headers: scopedHeaders() })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Request failed: ${response.status}`))
  }
  return await response.json() as T
}

async function sendJSON<T>(path: string, method: string, body: unknown): Promise<T> {
  const response = await fetch(path, { method, headers: scopedHeaders({ 'Content-Type': 'application/json' }), body: JSON.stringify(body) })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Request failed: ${response.status}`))
  }
  return await response.json() as T
}

export async function getHealth(): Promise<HealthResponse> {
  return getJSON<HealthResponse>('/api/health')
}

function scopedHeaders(extra: Record<string, string> = {}): Record<string, string> {
  return { 'X-Knowledge-Base-ID': getSelectedKnowledgeBaseID(), ...extra }
}

export async function getKnowledgeBases(): Promise<KnowledgeBase[]> {
  const response = await getJSON<{ knowledge_bases: KnowledgeBase[] }>('/api/knowledge-bases')
  return response.knowledge_bases ?? []
}
export function createKnowledgeBase(input: KnowledgeBaseInput): Promise<KnowledgeBase> { return postJSON('/api/knowledge-bases', input) }
export function updateKnowledgeBase(id: string, input: KnowledgeBaseInput): Promise<KnowledgeBase> { return sendJSON(`/api/knowledge-bases/${encodeURIComponent(id)}`, 'PUT', input) }
export async function archiveKnowledgeBase(id: string): Promise<void> {
  const response = await fetch(`/api/knowledge-bases/${encodeURIComponent(id)}/archive`, { method: 'POST', headers: scopedHeaders() })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Request failed: ${response.status}`))
  }
}

export function getConfig(): Promise<ConfigStatus> { return getJSON('/api/config/status') }
export function saveSettings(value: SettingsInput): Promise<ConfigStatus & { reindex_required: boolean }> { return sendJSON('/api/config/settings', 'PUT', value) }
export function reindex(): Promise<{ wiki_passages: number; source_chunks: number; embeddings_made: number }> { return postJSON('/api/indexes/reindex', {}) }

export async function getSources(): Promise<SourceDocument[]> {
  const response = await getJSON<{ sources: SourceDocument[] }>('/api/sources')
  return response.sources ?? []
}

export async function uploadSource(file: File): Promise<SourceUpload> {
  const body = new FormData()
  body.append('file', file)
  const response = await fetch('/api/sources', { method: 'POST', headers: scopedHeaders(), body })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Upload failed: ${response.status}`))
  }
  return await response.json() as SourceUpload
}

export async function deleteSource(id: string): Promise<{ deleted_pages: number; deleted_claims: number }> {
  const response = await fetch(`/api/sources/${encodeURIComponent(id)}`, { method: 'DELETE', headers: scopedHeaders() })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Delete failed: ${response.status}`))
  }
  return await response.json() as { deleted_pages: number; deleted_claims: number }
}

export async function deleteWikiPage(key: string): Promise<void> {
  const response = await fetch(`/api/wiki/pages/${encodeURIComponent(key)}`, { method: 'DELETE', headers: scopedHeaders() })
  if (!response.ok) {
    const payload = await response.json().catch(() => null) as ErrorPayload | null
    throw new Error(errorMessage(payload, `Delete failed: ${response.status}`))
  }
}

export async function getSourceChunks(id: string): Promise<SourceChunk[]> {
  const response = await getJSON<{ chunks: SourceChunk[] }>(`/api/sources/${encodeURIComponent(id)}/chunks`)
  return response.chunks ?? []
}
export function getSourceChunk(id: string): Promise<SourceChunkDetail> { return getJSON(`/api/source-chunks/${encodeURIComponent(id)}`) }
export function getSourceWiki(id: string): Promise<SourceWikiTrace> { return getJSON(`/api/sources/${encodeURIComponent(id)}/wiki`) }

export async function getCompilations(): Promise<CompilationRun[]> {
  const response = await getJSON<{ compilations: CompilationRun[] }>('/api/compilations')
  return response.compilations
}
export function getCompilation(id: string): Promise<CompilationRun> { return getJSON(`/api/compilations/${encodeURIComponent(id)}`) }
export function compileSource(documentID: string): Promise<{ run_id: string; status: string }> { return postJSON('/api/compilations', { document_id: documentID }) }
export function retryCompilationRender(id: string): Promise<CompilationRun> { return postJSON(`/api/compilations/${encodeURIComponent(id)}/render`, {}) }

export async function getWikiPages(pageType?: PageType): Promise<WikiPageListItem[]> {
  const query = pageType ? `?type=${pageType}` : ''
  const response = await getJSON<{ pages: WikiPageListItem[] }>(`/api/wiki/pages${query}`)
  return response.pages
}

export function getWikiPage(slug: string): Promise<WikiPageDetail> {
  return getJSON<WikiPageDetail>(`/api/wiki/pages/${encodeURIComponent(slug)}`)
}

export function getWikiGraph(): Promise<WikiGraph> {
  return getJSON<WikiGraph>('/api/wiki/graph')
}

export async function getWikiRevisions(slug: string): Promise<RevisionView[]> {
  const response = await getJSON<{ revisions: RevisionView[] }>(`/api/wiki/pages/${encodeURIComponent(slug)}/revisions`)
  return response.revisions
}

export function searchWiki(query: string): Promise<RetrievalResult> {
  return getJSON<RetrievalResult>(`/api/retrieval/search?q=${encodeURIComponent(query)}`)
}

export function askQuestion(question: string): Promise<QueryResult> {
  return postJSON<QueryResult>('/api/query', { question })
}

export function createConversation(title = ''): Promise<Conversation> {
  return postJSON<Conversation>('/api/conversations', { title })
}

export async function getConversations(): Promise<Conversation[]> {
  const response = await getJSON<{ conversations: Conversation[] }>('/api/conversations')
  return response.conversations
}

export async function getConversation(id: string): Promise<ConversationDetail> {
  return getJSON<ConversationDetail>(`/api/conversations/${encodeURIComponent(id)}`)
}

export function sendMessage(id: string, question: string): Promise<ChatTurnResult> {
  return postJSON(`/api/conversations/${encodeURIComponent(id)}/messages`, { question })
}

export async function streamMessage(id: string, question: string, onDelta: (delta: string) => void): Promise<ChatTurnResult> {
  const response = await fetch(`/api/conversations/${encodeURIComponent(id)}/messages/stream`, {
    method: 'POST',
    headers: scopedHeaders({ 'Content-Type': 'application/json', Accept: 'text/event-stream' }),
    body: JSON.stringify({ question }),
  })
  if (!response.ok) {
    const payload = sseErrorPayload(await response.text().catch(() => ''))
    throw new Error(errorMessage(payload, `Request failed: ${response.status}`))
  }
  if (!response.body) throw new Error(`Request failed: ${response.status}`)

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let completed: ChatTurnResult | undefined
  const consume = (block: string) => {
    const event = block.split('\n').find(line => line.startsWith('event:'))?.slice(6).trim()
    const data = block.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n')
    if (!event || !data) return
    const payload = JSON.parse(data) as { delta?: string } | ErrorPayload | ChatTurnResult
    if (event === 'message' && 'delta' in payload && payload.delta) onDelta(payload.delta)
    if (event === 'done') completed = payload as ChatTurnResult
    if (event === 'error') throw new Error(errorMessage(payload as ErrorPayload, 'Stream failed'))
  }

  while (true) {
    const { value, done } = await reader.read()
    buffer += decoder.decode(value, { stream: !done })
    const blocks = buffer.split('\n\n')
    buffer = blocks.pop() ?? ''
    blocks.forEach(consume)
    if (done) break
  }
  if (buffer.trim()) consume(buffer)
  if (!completed) throw new Error('Stream ended before completion')
  return completed
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
	return sendJSON(path, 'POST', body)
}
