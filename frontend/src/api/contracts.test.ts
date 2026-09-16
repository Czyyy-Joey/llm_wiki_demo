import { describe, expect, it, vi } from 'vitest'
import { getHealth, getWikiPage, getWikiPages, getWikiRevisions, searchWiki, streamMessage } from './contracts'

describe('API contracts', () => { it('reads the backend response body directly', async () => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ status: 'ok', database: 'ok', providers: { compiler_llm: { configured: false, endpoint_configured: false }, embedding: { configured: false, endpoint_configured: false } } }) })); await expect(getHealth()).resolves.toMatchObject({ status: 'ok', database: 'ok' }); vi.unstubAllGlobals() }) })

describe('API errors', () => {
  it('surfaces the backend diagnostic message and request ID', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 422, json: async () => ({ error: { code: 'unprocessable_entity', message: 'provider unavailable', request_id: 'req-ui-test' } }) }))
    await expect(getHealth()).rejects.toThrow('provider unavailable (request_id: req-ui-test)')
    vi.unstubAllGlobals()
  })

  it('surfaces request IDs from non-success SSE responses', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 404, text: async () => 'event: error\ndata: {"error":{"code":"not_found","message":"conversation not found","request_id":"req-sse-ui"}}\n\n' }))
    await expect(streamMessage('missing', 'hello', () => undefined)).rejects.toThrow('conversation not found (request_id: req-sse-ui)')
    vi.unstubAllGlobals()
  })
})

describe('Wiki API contracts', () => {
  it('unwraps the real page list and revision response wrappers', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ pages: [{ id: 'page-1', slug: 'topic', page_type: 'topic', title: 'Topic', summary: 'Summary', status: 'active', current_revision: 1, claim_count: 1, source_count: 1, link_count: 0 }] }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ revisions: [{ revision: { revision_number: 1 }, diff: { added_claims: ['claim-1'], removed_claims: [], changed: [] } }] }) })
    vi.stubGlobal('fetch', fetchMock)
    await expect(getWikiPages('topic')).resolves.toHaveLength(1)
    await expect(getWikiRevisions('topic')).resolves.toHaveLength(1)
    expect(fetchMock).toHaveBeenNthCalledWith(1, '/api/wiki/pages?type=topic', { headers: { 'X-Knowledge-Base-ID': 'default' } })
    expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/wiki/pages/topic/revisions', { headers: { 'X-Knowledge-Base-ID': 'default' } })
    vi.unstubAllGlobals()
  })

  it('reads Wiki page detail directly without a body wrapper', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ page: { id: 'page-1', slug: 'topic' }, sections: [], claims: [], links: [], backlinks: [], related: [], sources: [] }) }))
    await expect(getWikiPage('topic')).resolves.toMatchObject({ page: { slug: 'topic' } })
    vi.unstubAllGlobals()
  })

  it('reads retrieval scores and trace directly from the search response', async () => {
    const payload = {
      query: 'semantic search',
      candidates: [{ passage_id: 'passage-1', page_id: 'page-1', slug: 'vector-databases', title: 'Vector Databases', page_type: 'concept', text: 'Compiled knowledge', fts_score: 1, vector_score: 0.8, rrf_score: 0.03, expansion_score: 0, final_score: 0.12, expanded: false, selected: true }],
      context: [{ id: 'passage-1', kind: 'wiki', page_id: 'page-1', passage_id: 'passage-1', text: 'Compiled knowledge', citation: 'vector-databases' }],
      trace: { id: 'trace-1', normalized_query: 'semantic search', fts_candidates: ['page-1'], vector_candidates: ['page-1'], rrf_score: { 'page-1': 0.03 }, expanded_pages: [] },
    }
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => payload })
    vi.stubGlobal('fetch', fetchMock)
    await expect(searchWiki('semantic search')).resolves.toMatchObject({ candidates: [{ vector_score: 0.8 }], trace: { id: 'trace-1' } })
    expect(fetchMock).toHaveBeenCalledWith('/api/retrieval/search?q=semantic%20search', { headers: { 'X-Knowledge-Base-ID': 'default' } })
    vi.unstubAllGlobals()
  })
})
