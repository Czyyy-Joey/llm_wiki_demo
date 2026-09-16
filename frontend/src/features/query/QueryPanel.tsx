import { ExternalLink, Search, Sparkles } from 'lucide-react'
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { askQuestion } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

export function QueryPanel() {
  const { t } = useI18n()
  const [question, setQuestion] = useState('')
  const [submitted, setSubmitted] = useState('')
  const workspace = useWorkspace()
  const result = useQuery({ queryKey: ['answer', workspace.selectedID, submitted], queryFn: () => askQuestion(submitted), enabled: submitted.length > 0 })
  return <main className="work-page query-page"><header className="page-head"><div><p className="eyebrow">{t('Grounded query')}</p><h1>{t('Ask once, cite the Wiki')}</h1><p className="muted">{t('Answers use the same hybrid retrieval context as Search.')}</p></div><Sparkles size={21} /></header><form className="query-form" onSubmit={event => { event.preventDefault(); setSubmitted(question.trim()) }}><Search size={18} /><input value={question} onChange={event => setQuestion(event.target.value)} placeholder={t('Ask a grounded question')} aria-label={t('Ask a grounded question')} /><button type="submit" disabled={!question.trim() || result.isFetching}>{t('Ask')}</button></form>{result.isError && <div className="notice error"><span>{result.error.message}</span><button onClick={() => void result.refetch()}>{t('Retry')}</button></div>}{result.isFetching && <p className="inline-state">{t('Retrieving Wiki context and generating a cited answer...')}</p>}{result.data && <><section className="answer-panel"><p className="eyebrow">{t('Answer')}</p><p className="answer-text">{result.data.answer}</p><div className="answer-citations"><strong>{t('Source citations')}</strong>{result.data.citations.length ? result.data.citations.map(citation => <a href={`/sources?chunk=${encodeURIComponent(citation.source_chunk_id || citation.id)}`} key={citation.id}><ExternalLink size={13} />[{citation.label || citation.id}]</a>) : <span className="muted">{t('No source evidence')}</span>}</div><p className="muted">{t('Trace')} {result.data.retrieval_trace.id} · {result.data.context.length} {t('context items')}</p></section><section className="answer-grid"><div><h2>{t('Wiki references')}</h2>{result.data.wiki_references.map(reference => <LinkCard key={reference.id} title={reference.label || reference.page_id || reference.id} text={reference.text} href={reference.page_id ? `/wiki/${reference.page_id}` : '/wiki'} />)}</div><div><h2>{t('Context used')}</h2>{result.data.context.map(item => <div className="context-row" key={item.id}><span className="context-kind">{item.kind}</span><p>{item.text}</p><small>{item.id}</small></div>)}</div></section></>}</main>
}

function LinkCard({ title, text, href }: { title: string; text: string; href: string }) { return <a className="reference-card" href={href}><strong>{title}</strong><p>{text}</p></a> }
