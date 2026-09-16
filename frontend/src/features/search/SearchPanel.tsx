import { ChevronDown, Search, SlidersHorizontal } from 'lucide-react'
import { useState } from 'react'
import type { FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { searchWiki } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

export function SearchPanel() {
  const { t } = useI18n()
  const [query, setQuery] = useState('')
  const [submitted, setSubmitted] = useState('')
  const workspace = useWorkspace()
  const result = useQuery({ queryKey: ['retrieval', workspace.selectedID, submitted], queryFn: () => searchWiki(submitted), enabled: submitted.length > 0 })
  function submit(event: FormEvent) { event.preventDefault(); setSubmitted(query.trim()) }
  return <main className="work-page search-page">
    <section className="page-head"><div><p className="eyebrow">{t('Hybrid retrieval')}</p><h1>{t('Search compiled Wiki')}</h1><p className="muted">{t('Wiki passages lead retrieval; Source chunks are added only as evidence fallback.')}</p></div><SlidersHorizontal size={20} /></section>
    <form className="search-form" onSubmit={submit}><Search size={18} /><input value={query} onChange={event => setQuery(event.target.value)} placeholder={t('Search a concept, method, or claim')} aria-label={t('Search compiled Wiki')} /><button type="submit" disabled={!query.trim()}>{t('Search')}</button></form>
    {result.isError && <div className="notice error"><span>{result.error instanceof Error ? result.error.message : t('Search failed')}</span><button onClick={() => void result.refetch()}>{t('Retry')}</button></div>}
    {result.isLoading && <p className="inline-state">{t('Searching FTS and vectors...')}</p>}
    {result.data && <div className="search-grid"><section><div className="section-title"><h2>{t('Ranked candidates')}</h2><span>{result.data.candidates.length} {t('pages')}</span></div>{result.data.candidates.map(candidate => <article className="result-row" key={candidate.page_id}><div className="result-heading"><div><h3>{candidate.title}</h3><span className="type-mark">{candidate.page_type}</span></div>{candidate.selected ? <strong className="selected-mark">{t('In context')}</strong> : <span className="muted">{t('Dropped')}</span>}</div><p>{candidate.text}</p><div className="score-line"><span>FTS <b>{candidate.fts_score.toFixed(3)}</b></span><span>{t('Vector')} <b>{candidate.vector_score.toFixed(3)}</b></span><span>RRF <b>{candidate.rrf_score.toFixed(4)}</b></span><span>{t('Expansion')} <b>{candidate.expansion_score.toFixed(3)}</b></span><span>{t('Final')} <b>{candidate.final_score.toFixed(4)}</b></span>{candidate.expanded && <span className="expanded-mark">{t('1-hop link expansion')}</span>}</div>{candidate.discard_reason && <small className="discard">{candidate.discard_reason}</small>}</article>)}{result.data.candidates.length === 0 && <div className="empty-panel">{t('No active Wiki candidate matched this query.')}</div>}<div className="section-title context-title"><h2>{t('Final context')}</h2><span>{result.data.context.length} {t('items')} · {result.data.trace.context_used} {t('chars')}</span></div>{result.data.context.map(item => <article className="context-row" key={item.id}><span className="context-kind">{item.kind}</span><p>{item.text}</p><small>{item.id}</small></article>)}</section><aside className="trace-panel"><div className="section-title"><h2>{t('Retrieval trace')}</h2><ChevronDown size={16} /></div><TraceLine label={t('FTS candidates')} value={result.data.trace.fts_candidates.join(', ') || t('none')} /><TraceLine label={t('Vector candidates')} value={result.data.trace.vector_candidates.join(', ') || t('none')} /><TraceLine label={t('Link expansion')} value={result.data.trace.expanded_pages.join(', ') || t('none')} /><TraceLine label={t('Final candidates')} value={result.data.trace.final_candidates?.join(', ') || t('none')} /><TraceLine label={t('Dropped')} value={result.data.trace.dropped_candidates?.join(', ') || t('none')} /><TraceLine label={t('Context IDs')} value={result.data.trace.context_ids?.join(', ') || t('none')} />{result.data.trace.notes?.map(note => <p className="trace-note" key={note}>{note}</p>)}</aside></div>}
  </main>
}

function TraceLine({ label, value }: { label: string; value: string }) { return <div className="trace-line"><small>{label}</small><p>{value}</p></div> }
