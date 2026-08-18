import { Search, SlidersHorizontal } from 'lucide-react'
import { useState } from 'react'
import type { FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { searchWiki } from '../../api/contracts'

export function SearchPanel() {
  const [query, setQuery] = useState('')
  const [submitted, setSubmitted] = useState('')
  const result = useQuery({ queryKey: ['retrieval', submitted], queryFn: () => searchWiki(submitted), enabled: submitted.length > 0 })
  function submit(event: FormEvent) { event.preventDefault(); setSubmitted(query.trim()) }
  return <main className="search-page">
    <section className="search-head"><div><p className="eyebrow">Phase 4 retrieval</p><h1>Search compiled Wiki</h1><p className="muted">Wiki passages lead retrieval; source chunks appear as evidence.</p></div><SlidersHorizontal size={20} /></section>
    <form className="search-form" onSubmit={submit}><Search size={18} /><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Search a concept, method, or claim" aria-label="Search compiled Wiki" /><button type="submit" disabled={!query.trim()}>Search</button></form>
    {result.isError && <p className="error-text">{result.error instanceof Error ? result.error.message : 'Search failed'}</p>}
    {result.isLoading && <p className="muted">Searching FTS and vectors...</p>}
    {result.data && <div className="search-grid"><section><div className="section-title"><h2>Ranked candidates</h2><span>{result.data.candidates.length} pages</span></div>{result.data.candidates.map(candidate => <article className="result-row" key={candidate.page_id}><div><h3>{candidate.title}</h3><p>{candidate.text}</p><div className="score-line"><span>FTS {candidate.fts_score.toFixed(3)}</span><span>Vector {candidate.vector_score.toFixed(3)}</span><span>RRF {candidate.rrf_score.toFixed(4)}</span><span>Expansion {candidate.expansion_score.toFixed(3)}</span><span>Final {candidate.final_score.toFixed(4)}</span>{candidate.expanded && <span>Expanded</span>}</div></div></article>)}</section><aside className="trace-panel"><div className="section-title"><h2>Retrieval trace</h2></div><p><strong>FTS:</strong> {result.data.trace.fts_candidates.join(', ') || 'none'}</p><p><strong>Vector:</strong> {result.data.trace.vector_candidates.join(', ') || 'none'}</p><p><strong>Expansion:</strong> {result.data.trace.expanded_pages.join(', ') || 'none'}</p><p><strong>Context:</strong> {result.data.trace.context_ids?.length ?? 0} items</p>{result.data.trace.notes?.map(note => <p className="muted" key={note}>{note}</p>)}</aside></div>}
  </main>
}
