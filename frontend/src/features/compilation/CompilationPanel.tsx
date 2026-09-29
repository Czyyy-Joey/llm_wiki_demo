import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, CircleDashed, FileDiff, RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { compileSource, getCompilations, getSources, retryCompilationRender, type CompilationRun } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

const stages = ['Parse', 'Analyze', 'Match', 'Plan', 'Validate', 'Apply']
const terminalRunStatuses = new Set(['applied', 'failed', 'render_pending'])
function completeStage(run: CompilationRun, stage: string) {
  if (stage === 'Parse') return true
  if (stage === 'Analyze') return Boolean(run.analyze)
  if (stage === 'Match') return Boolean(run.candidates)
  if (stage === 'Plan') return Boolean(run.plan)
  if (stage === 'Validate') return Boolean(run.validation?.valid)
  return run.status === 'applied' || run.status === 'render_pending'
}

export function CompilationPanel() {
  const client = useQueryClient()
  const { t } = useI18n()
  const workspace = useWorkspace()
  const [params, setParams] = useSearchParams()
  const runs = useQuery({
    queryKey: ['compilations', workspace.selectedID],
    queryFn: getCompilations,
    refetchInterval: query => ((query.state.data as CompilationRun[] | undefined)?.some(run => !terminalRunStatuses.has(run.status)) ? 1500 : false),
  })
  const sources = useQuery({ queryKey: ['sources', workspace.selectedID], queryFn: getSources })
  const [selected, setSelected] = useState(params.get('run') ?? '')
  const [sourceID, setSourceID] = useState(params.get('source') ?? '')
  // Compiling or re-rendering rewrites Wiki pages, links, and the graph, so
  // refresh those views too — not only the run list.
  const invalidateContent = () => {
    for (const key of [['compilations'], ['wiki-pages'], ['wiki-page'], ['wiki-revisions'], ['wiki-graph'], ['source-wiki'], ['sources']]) {
      void client.invalidateQueries({ queryKey: key })
    }
  }
  const compile = useMutation({ mutationFn: compileSource, onSuccess: value => { setSelected(value.run_id); setParams({ run: value.run_id }); invalidateContent() } })
  const retry = useMutation({ mutationFn: retryCompilationRender, onSuccess: () => invalidateContent() })
  useEffect(() => { if (!selected && runs.data?.[0]) setSelected(runs.data[0].id) }, [runs.data, selected])
  const run = runs.data?.find(item => item.id === selected)
  const sourceName = (id: string) => sources.data?.find(item => item.id === id)?.original_name ?? id
  // Server-derived compile state so a refresh mid-run keeps the button locked.
  const isCompiling = (docID: string) => (compile.isPending && compile.variables === docID) || !terminalRunStatuses.has((runs.data?.find(item => item.document_id === docID)?.status) ?? 'applied')
  return <main className="work-page compilation-page"><header className="page-head"><div><p className="eyebrow">{t('Knowledge compiler')}</p><h1>{t('Compilation')}</h1><p className="muted">{t('Observe Parse → Analyze → Match → Plan → Validate → Apply without hidden writes.')}</p></div><div className="compile-control"><select aria-label={t('Source to compile')} value={sourceID} onChange={event => setSourceID(event.target.value)}><option value="">{t('Select parsed Source')}</option>{sources.data?.filter(item => item.status === 'parsed').map(item => <option value={item.id} key={item.id}>{item.original_name}</option>)}</select><button disabled={!sourceID || isCompiling(sourceID)} onClick={() => compile.mutate(sourceID)}>{t(isCompiling(sourceID) ? 'Running...' : 'Compile')}</button></div></header>
    {(compile.isError || retry.isError) && <div className="notice error"><span>{compile.error?.message ?? retry.error?.message}</span><button onClick={() => { compile.reset(); retry.reset() }}>{t('Dismiss')}</button></div>}
    <div className="compilation-layout"><aside className="run-list"><div className="panel-title"><span>{t('Compilation runs')}</span><small>{runs.data?.length ?? 0}</small></div>{runs.isLoading && <p className="inline-state">{t('Loading runs...')}</p>}{runs.isError && <button className="retry-button" onClick={() => void runs.refetch()}><RefreshCw size={14} />{t('Retry')}</button>}{runs.data?.map(item => <button className={item.id === selected ? 'run-row active' : 'run-row'} onClick={() => { setSelected(item.id); setParams({ run: item.id }) }} key={item.id}><span><strong>{sourceName(item.document_id)}</strong><small>{new Date(item.created_at).toLocaleString()}</small></span><span className={`status status-${item.status}`}>{t(item.status)}</span></button>)}{!runs.isLoading && runs.data?.length === 0 && <p className="inline-state">{t('No compilation runs yet.')}</p>}</aside>
      <section className="run-detail">{run ? <><div className="detail-head"><div><span className={`status status-${run.status}`}>{run.status}</span><h2>{sourceName(run.document_id)}</h2><p>{run.id} · {run.model || 'unknown model'}</p></div>{run.status === 'render_pending' && <button className="primary-command" disabled={retry.isPending} onClick={() => retry.mutate(run.id)}>Retry render</button>}</div>
        <div className="pipeline">{stages.map(stage => <div className={completeStage(run, stage) ? 'done' : ''} key={stage}>{completeStage(run, stage) ? <CheckCircle2 size={16} /> : <CircleDashed size={16} />}<span>{t(stage)}</span></div>)}</div>
        {run.error && <div className="notice error">{run.error}</div>}
        <RunSection title={t('Analyze')} ready={Boolean(run.analyze)} readyLabel={t('captured')} pendingLabel={t('pending')} emptyLabel={t('No output captured at this stage.')}><p>{run.analyze?.summary}</p><div className="topic-grid">{run.analyze?.topics?.map(topic => <article key={topic.key}><span className="type-mark">{topic.page_type}</span><h3>{topic.title}</h3><small>{topic.claims.length} {t('claims')}</small></article>)}</div></RunSection>
        <RunSection title={t('Existing Wiki Candidates')} ready={Boolean(run.candidates)} readyLabel={t('captured')} pendingLabel={t('pending')} emptyLabel={t('No output captured at this stage.')}>{run.candidates?.map(candidate => <div className="candidate-row" key={`${candidate.topic_key}-${candidate.page_id}`}><span>{candidate.topic_key} → {candidate.title}</span><strong>{candidate.score.toFixed(3)}</strong></div>)}</RunSection>
        <RunSection title={t('Plan')} ready={Boolean(run.plan)} readyLabel={t('captured')} pendingLabel={t('pending')} emptyLabel={t('No output captured at this stage.')}>{run.plan?.page_actions.map((action, index) => <article className="plan-action" key={`${action.action}-${index}`}><div><strong>{action.action}</strong><span>{action.title || action.target_page_id || action.source_page_id}</span></div><p>{action.reason}</p><small>{action.claim_actions?.length ?? 0} {t('claim actions')}</small></article>)}</RunSection>
        <RunSection title={t('Validation')} ready={Boolean(run.validation)} readyLabel={t('captured')} pendingLabel={t('pending')} emptyLabel={t('No output captured at this stage.')}><div className={run.validation?.valid ? 'validation valid' : 'validation invalid'}>{run.validation?.valid ? t('Plan passed schema and domain validation.') : run.validation?.error}</div></RunSection>
        <RunSection title={t('Diff & Apply')} ready={Boolean(run.diff || run.apply_result)} readyLabel={t('captured')} pendingLabel={t('pending')} emptyLabel={t('No output captured at this stage.')}><div className="json-summary"><FileDiff size={17} /><pre>{JSON.stringify(run.diff ?? run.apply_result, null, 2)}</pre></div>{run.plan?.page_actions.map(action => action.slug && <Link key={action.slug} to={`/wiki/${action.slug}`}>{t('Open')} {action.title || action.slug}</Link>)}</RunSection>
      </> : <div className="inline-state">{t('Select a run to inspect the compiler pipeline.')}</div>}</section></div>
  </main>
}
function RunSection({ title, ready, readyLabel, pendingLabel, emptyLabel, children }: { title: string; ready: boolean; readyLabel: string; pendingLabel: string; emptyLabel: string; children: React.ReactNode }) { return <section className="run-section"><div className="panel-title"><span>{title}</span><small>{ready ? readyLabel : pendingLabel}</small></div>{ready ? children : <p className="inline-state">{emptyLabel}</p>}</section> }
