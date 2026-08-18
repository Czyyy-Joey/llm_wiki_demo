export type ProviderStatus = { configured: boolean; endpoint_configured: boolean; model?: string }
export type HealthResponse = { status: string; database: string; providers: { compiler_llm: ProviderStatus; embedding: ProviderStatus } }
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
export type RevisionDiff = { added_claims: string[]; removed_claims: string[]; changed: string[] }
export type RevisionView = { revision: { page_id: string; revision_number: number; compilation_run_id: string; change_summary: string; created_at: string }; diff: RevisionDiff }
export type RetrievalCandidate = { passage_id: string; page_id: string; slug: string; title: string; page_type: string; text: string; fts_score: number; vector_score: number; rrf_score: number; expansion_score: number; final_score: number; expanded: boolean; expansion_from?: string[]; selected: boolean; discard_reason?: string }
export type RetrievalTrace = { id: string; normalized_query: string; fts_candidates: string[]; vector_candidates: string[]; rrf_score: Record<string, number>; expanded_pages: string[]; final_candidates?: string[]; dropped_candidates?: string[]; context_ids?: string[]; context_budget: number; context_used: number; notes?: string[] }
export type RetrievalContext = { id: string; kind: string; page_id?: string; passage_id?: string; source_chunk_id?: string; text: string; citation: string }
export type RetrievalResult = { query: string; candidates: RetrievalCandidate[]; context: RetrievalContext[]; trace: RetrievalTrace }

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path)
  if (!response.ok) throw new Error(`Request failed: ${response.status}`)
  return await response.json() as T
}

export async function getHealth(): Promise<HealthResponse> {
  return getJSON<HealthResponse>('/api/health')
}

export async function getWikiPages(pageType?: PageType): Promise<WikiPageListItem[]> {
  const query = pageType ? `?type=${pageType}` : ''
  const response = await getJSON<{ pages: WikiPageListItem[] }>(`/api/wiki/pages${query}`)
  return response.pages
}

export function getWikiPage(slug: string): Promise<WikiPageDetail> {
  return getJSON<WikiPageDetail>(`/api/wiki/pages/${encodeURIComponent(slug)}`)
}

export async function getWikiRevisions(slug: string): Promise<RevisionView[]> {
  const response = await getJSON<{ revisions: RevisionView[] }>(`/api/wiki/pages/${encodeURIComponent(slug)}/revisions`)
  return response.revisions
}

export function searchWiki(query: string): Promise<RetrievalResult> {
  return getJSON<RetrievalResult>(`/api/retrieval/search?q=${encodeURIComponent(query)}`)
}
