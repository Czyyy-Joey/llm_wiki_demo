import { ChevronDown, Search, SlidersHorizontal } from 'lucide-react'
import { useState } from 'react'
import type { FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { searchWiki } from '../../api/contracts'

export function SearchPanel() {
  const [query, setQuery] = useState('')
  const [submitted, setSubmitted] = useState('')
  const result = useQuery({ queryKey: ['retrieval', submitted], queryFn: () => searchWiki(submitted), enabled: submitted.length > 0 })
  function submit(event: FormEvent) { event.preventDefault(); setSubmitted(query.trim()) }
  return <main className="work-page search-page">
    <section className="page-head"><div><p className="eyebrow">Hybrid retrieval</p><h1>Search compiled Wiki</h1><p className="muted">Wiki passages lead retrieval; Source chunks are added only as evidence fallback.</p></div><SlidersHorizontal size={20} /></section>
    <form className="search-form" onSubmit={submit}><Search size={18} /><input value={query} onChange={event => setQuery(event.target.value)} placeholder="Search a concept, method, or claim" aria-label="Search compiled Wiki" /><button type="submit" disabled={!query.trim()}>Search</button></form>
    {result.isError && <div className="notice error"><span>{result.error instanceof Error ? result.error.message : 'Search failed'}</span><button onClick={() => void result.refetch()}>Retry</button></div>}
    {result.isLoading && <p className="inline-state">Searching FTS and vectors...</p>}
    {result.data && <div className="search-grid"><section><div className="section-title"><h2>Ranked candidates</h2><span>{result.data.candidates.length} pages</span></div>{result.data.candidates.map(candidate => <article className="result-row" key={candidate.page_id}><div className="result-heading"><div><h3>{candidate.title}</h3><span className="type-mark">{candidate.page_type}</span></div>{candidate.selected ? <strong className="selected-mark">In context</strong> : <span className="muted">Dropped</span>}</div><p>{candidate.text}</p><div className="score-line"><span>FTS <b>{candidate.fts_score.toFixed(3)}</b></span><span>Vector <b>{candidate.vector_score.toFixed(3)}</b></span><span>RRF <b>{candidate.rrf_score.toFixed(4)}</b></span><span>Expansion <b>{candidate.expansion_score.toFixed(3)}</b></span><span>Final <b>{candidate.final_score.toFixed(4)}</b></span>{candidate.expanded && <span className="expanded-mark">1-hop link expansion</span>}</div>{candidate.discard_reason && <small className="discard">{candidate.discard_reason}</small>}</article>)}{result.data.candidates.length === 0 && <div className="empty-panel">No active Wiki candidate matched this query.</div>}<div className="section-title context-title"><h2>Final context</h2><span>{result.data.context.length} items · {result.data.trace.context_used} chars</span></div>{result.data.context.map(item => <article className="context-row" key={item.id}><span className="context-kind">{item.kind}</span><p>{item.text}</p><small>{item.id}</small></article>)}</section><aside className="trace-panel"><div className="section-title"><h2>Retrieval trace</h2><ChevronDown size={16} /></div><TraceLine label="FTS candidates" value={result.data.trace.fts_candidates.join(', ') || 'none'} /><TraceLine label="Vector candidates" value={result.data.trace.vector_candidates.join(', ') || 'none'} /><TraceLine label="Link expansion" value={result.data.trace.expanded_pages.join(', ') || 'none'} /><TraceLine label="Final candidates" value={result.data.trace.final_candidates?.join(', ') || 'none'} /><TraceLine label="Dropped" value={result.data.trace.dropped_candidates?.join(', ') || 'none'} /><TraceLine label="Context IDs" value={result.data.trace.context_ids?.join(', ') || 'none'} />{result.data.trace.notes?.map(note => <p className="trace-note" key={note}>{note}</p>)}</aside></div>}
  </main>
}

function TraceLine({ label, value }: { label: string; value: string }) { return <div className="trace-line"><small>{label}</small><p>{value}</p></div> }
