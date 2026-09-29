import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, CheckCircle2, FileText, FolderUp, RefreshCw, Upload } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { compileSource, deleteSource, deleteSources, getCompilations, getSourceChunk, getSourceChunks, getSources, getSourceWiki, uploadSources, type BatchUploadProgress, type BatchUploadResult, type CompilationRun, type SourceChunk } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

const supportedExtensions = ['.md', '.txt', '.pdf']
// A compilation run in one of these states is finished; anything else means a
// run is still working, so its source must not be recompiled concurrently.
const terminalRunStatuses = new Set(['applied', 'failed', 'render_pending'])
// Compiling or deleting a source rewrites the Wiki, its graph, revisions, and
// the source's own trace, so every dependent view must refresh, not only the
// source list. Partial keys match all workspace-scoped variants.
const contentQueryKeys = [['sources'], ['source-chunks'], ['source-wiki'], ['source-chunk'], ['wiki-pages'], ['wiki-page'], ['wiki-revisions'], ['wiki-graph'], ['compilations']]
// Per-source compile state for this tab's queue. Compiles run one at a time so
// clicking several sources leaves the rest 'queued' (pending) rather than
// firing overlapping long requests or being silently ignored.
type CompileEntry = { state: 'queued' | 'compiling' | 'done' | 'error'; runId?: string; status?: string; error?: string }
function isSupportedSource(file: File) {
  const name = file.name.toLowerCase()
  return supportedExtensions.some(ext => name.endsWith(ext))
}
// webkitdirectory is a valid DOM attribute for folder pickers but is missing from React's input typings.
const directoryProps = { webkitdirectory: '', directory: '' } as Record<string, string>

function uploadSummary(result: BatchUploadResult, skipped: number, t: (key: string) => string) {
  const created = result.succeeded.filter(item => item.result.action !== 'NO_OP').length
  const duplicates = result.succeeded.length - created
  const parts: string[] = [`${created} ${t('uploaded')}`]
  if (duplicates > 0) parts.push(`${duplicates} ${t('duplicates reused')}`)
  if (result.failed.length > 0) parts.push(`${result.failed.length} ${t('failed')}`)
  if (skipped > 0) parts.push(`${skipped} ${t('unsupported skipped')}`)
  return parts.join(' · ')
}


function location(chunk: SourceChunk, t: (key: string) => string) {
  const parts = chunk.page_number ? [`${t('Page')} ${chunk.page_number}`] : []
  if (chunk.heading_path?.length) parts.push(chunk.heading_path.join(' / '))
  parts.push(`${t('Chars')} ${chunk.char_start}-${chunk.char_end}`)
  return parts.join(' · ')
}

export function SourcesPanel() {
  const client = useQueryClient()
  const { t } = useI18n()
  const [params, setParams] = useSearchParams()
  const workspace = useWorkspace()
  const [selectedID, setSelectedID] = useState(params.get('source') ?? '')
  const [selectedChunk, setSelectedChunk] = useState(params.get('chunk') ?? '')
  const [progress, setProgress] = useState<BatchUploadProgress | null>(null)
  const [skipped, setSkipped] = useState(0)
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [compileEntries, setCompileEntries] = useState<Record<string, CompileEntry>>({})
  const queueRef = useRef<string[]>([])
  const drainingRef = useRef(false)
  const invalidateContent = useCallback(() => {
    for (const key of contentQueryKeys) void client.invalidateQueries({ queryKey: key })
  }, [client])
  const sources = useQuery({ queryKey: ['sources', workspace.selectedID], queryFn: getSources })
  // Compile status lives on the server, not in this tab: poll runs while any is
  // in progress so a refresh mid-compile still shows "Compiling" and keeps the
  // button disabled, preventing a second run on the same source.
  const runs = useQuery({
    queryKey: ['compilations', workspace.selectedID],
    queryFn: getCompilations,
    refetchInterval: query => ((query.state.data as CompilationRun[] | undefined)?.some(run => !terminalRunStatuses.has(run.status)) ? 1500 : false),
  })
  const chunks = useQuery({ queryKey: ['source-chunks', workspace.selectedID, selectedID], queryFn: () => getSourceChunks(selectedID), enabled: Boolean(selectedID) })
  const trace = useQuery({ queryKey: ['source-wiki', workspace.selectedID, selectedID], queryFn: () => getSourceWiki(selectedID), enabled: Boolean(selectedID) })
  const directChunk = useQuery({ queryKey: ['source-chunk', workspace.selectedID, selectedChunk], queryFn: () => getSourceChunk(selectedChunk), enabled: Boolean(selectedChunk) })
  const upload = useMutation({
    mutationFn: (files: File[]) => uploadSources(files, setProgress),
    onSuccess: result => {
      void client.invalidateQueries({ queryKey: ['sources', workspace.selectedID] })
      const last = result.succeeded[result.succeeded.length - 1]
      if (last) { setSelectedID(last.source.id); setParams({ source: last.source.id }) }
      setProgress(null)
    },
    onError: () => setProgress(null),
  })
  function selectFiles(list: FileList | null) {
    const all = Array.from(list ?? [])
    const files = all.filter(isSupportedSource)
    setSkipped(all.length - files.length)
    upload.reset()
    if (files.length > 0) upload.mutate(files)
  }
  // Drain the compile queue one source at a time. The backend already
  // serializes per knowledge base; keeping the client serial too gives every
  // queued source an accurate 'queued' → 'compiling' → done/error progression.
  const drainQueue = useCallback(async () => {
    if (drainingRef.current) return
    drainingRef.current = true
    try {
      while (queueRef.current.length > 0) {
        const id = queueRef.current[0]
        setCompileEntries(prev => ({ ...prev, [id]: { state: 'compiling' } }))
        try {
          const result = await compileSource(id)
          setCompileEntries(prev => ({ ...prev, [id]: { state: 'done', runId: result.run_id, status: result.status } }))
          invalidateContent()
        } catch (error) {
          setCompileEntries(prev => ({ ...prev, [id]: { state: 'error', error: error instanceof Error ? error.message : String(error) } }))
        } finally {
          queueRef.current = queueRef.current.slice(1)
        }
      }
    } finally {
      drainingRef.current = false
    }
  }, [invalidateContent])
  const enqueueCompile = useCallback((id: string) => {
    if (queueRef.current.includes(id)) return
    queueRef.current = [...queueRef.current, id]
    setCompileEntries(prev => ({ ...prev, [id]: { state: 'queued' } }))
    void drainQueue()
  }, [drainQueue])
  const remove = useMutation({
    mutationFn: deleteSource,
    onSuccess: () => {
      invalidateContent()
      setSelectedID('')
      setSelectedChunk('')
      setParams({})
    },
  })
  const removeBatch = useMutation({
    mutationFn: deleteSources,
    onSuccess: (_result, ids) => {
      invalidateContent()
      if (ids.includes(selectedID)) { setSelectedID(''); setSelectedChunk(''); setParams({}) }
      setChecked(new Set())
    },
  })

  useEffect(() => {
    setSelectedID('')
    setSelectedChunk('')
    setChecked(new Set())
  }, [workspace.selectedID])

  useEffect(() => {
    if (!selectedID && sources.data?.[0]) setSelectedID(sources.data[0].id)
  }, [selectedID, sources.data])

  const source = sources.data?.find(item => item.id === selectedID) ?? directChunk.data?.source
  const visibleChunk = directChunk.data?.chunk ?? chunks.data?.find(item => item.id === selectedChunk)
  function selectSource(id: string) { setSelectedID(id); setSelectedChunk(''); setParams({ source: id }) }
  function inspect(id: string) { setSelectedChunk(id); setParams(selectedID ? { source: selectedID, chunk: id } : { chunk: id }) }
  // A source's compile state combines this tab's queue with the server's latest
  // run: 'queued' waits behind an earlier click, 'compiling' means a run is in
  // flight here or (after a refresh/other tab) not yet terminal on the server.
  const serverBusy = (docID: string) => !terminalRunStatuses.has((runs.data?.find(run => run.document_id === docID)?.status) ?? 'applied')
  const compileState = (docID: string): 'idle' | 'queued' | 'compiling' => {
    const entry = compileEntries[docID]
    if (entry?.state === 'queued') return 'queued'
    if (entry?.state === 'compiling' || serverBusy(docID)) return 'compiling'
    return 'idle'
  }
  function toggleChecked(id: string) { setChecked(current => { const next = new Set(current); if (next.has(id)) next.delete(id); else next.add(id); return next }) }
  const allIDs = sources.data?.map(item => item.id) ?? []
  const selectedIDs = allIDs.filter(id => checked.has(id))
  const allChecked = allIDs.length > 0 && selectedIDs.length === allIDs.length
  function toggleAll() { setChecked(allChecked ? new Set() : new Set(allIDs)) }
  function deleteChecked() {
    if (selectedIDs.length === 0) return
    if (window.confirm(`${t('Delete the selected sources and every page compiled only from them? This cannot be undone.')} (${selectedIDs.length})`)) removeBatch.mutate(selectedIDs)
  }

  return <main className="work-page sources-page">
    <header className="page-head"><div><p className="eyebrow">{t('Evidence layer')}</p><h1>{t('Sources')}</h1><p className="muted">{t('Preserved originals, parser status, stable chunks, and compilation entry points.')}</p></div>
      <div className="upload-actions">
        <label className={`upload-button ${upload.isPending ? 'disabled' : ''}`}><Upload size={16} />{t(upload.isPending ? 'Uploading...' : 'Upload files')}<input type="file" multiple accept=".md,.txt,.pdf,text/plain,text/markdown,application/pdf" disabled={upload.isPending} onChange={event => { selectFiles(event.target.files); event.target.value = '' }} /></label>
        <label className={`upload-button ${upload.isPending ? 'disabled' : ''}`}><FolderUp size={16} />{t('Upload folder')}<input type="file" multiple {...directoryProps} disabled={upload.isPending} onChange={event => { selectFiles(event.target.files); event.target.value = '' }} /></label>
      </div>
    </header>
    {progress && <div className="notice"><span>{t('Uploading')} {progress.done}/{progress.total}{progress.currentName ? ` · ${progress.currentName}` : ''}</span></div>}
    {upload.isError && <div className="notice error"><AlertCircle size={16} /><span>{upload.error.message}</span><button onClick={() => upload.reset()}>{t('Dismiss')}</button></div>}
    {!progress && upload.data && <div className={`notice ${upload.data.failed.length ? 'error' : 'success'}`}>{upload.data.failed.length ? <AlertCircle size={16} /> : <CheckCircle2 size={16} />}<span>{uploadSummary(upload.data, skipped, t)}</span><button onClick={() => { upload.reset(); setSkipped(0) }}>{t('Dismiss')}</button></div>}
    <div className="split-workspace">
      <aside className="record-list"><div className="panel-title"><span>{t('Source library')}</span><small>{sources.data?.length ?? 0}</small></div>
        {sources.data && sources.data.length > 0 && <div className="record-toolbar">
          <label className="select-all"><input type="checkbox" checked={allChecked} onChange={toggleAll} aria-label={t('Select all')} /><span>{t('Select all')}</span></label>
          {selectedIDs.length > 0 && <button className="danger-command small" disabled={removeBatch.isPending} onClick={deleteChecked}>{t(removeBatch.isPending ? 'Deleting...' : 'Delete selected')} ({selectedIDs.length})</button>}
        </div>}
        {removeBatch.isError && <div className="notice error"><AlertCircle size={16} /><span>{removeBatch.error.message}</span><button onClick={() => removeBatch.reset()}>{t('Dismiss')}</button></div>}
        {sources.isLoading && <State text={t('Loading Sources...')} retry={t('Retry')} />}
        {sources.isError && <State text={t('Could not load Sources.')} retry={t('Retry')} action={() => void sources.refetch()} />}
        {sources.data?.map(item => <div className={`record-row ${checked.has(item.id) ? 'checked' : ''}`} key={item.id}>
          <input type="checkbox" checked={checked.has(item.id)} onChange={() => toggleChecked(item.id)} aria-label={`${t('Select')} ${item.original_name}`} />
          <button className={item.id === selectedID ? 'record active' : 'record'} onClick={() => selectSource(item.id)}><FileText size={16} /><span><strong>{item.original_name}</strong><small>{item.media_type}</small></span>{compileState(item.id) === 'compiling' ? <span className="compile-badge compiling">{t('Compiling...')}</span> : compileState(item.id) === 'queued' ? <span className="compile-badge queued">{t('Queued')}</span> : <Status value={item.status} />}</button>
        </div>)}
        {!sources.isLoading && sources.data?.length === 0 && <State text={t('Upload Markdown, TXT, or a text PDF to begin.')} retry={t('Retry')} />}
      </aside>
      <section className="record-detail">{source ? <>
        <div className="detail-head"><div><Status value={source.status} /><h2>{source.original_name}</h2><p>{source.id}</p></div><div className="detail-actions">{source.status === 'parsed' && <button className="primary-command" disabled={compileState(source.id) !== 'idle'} onClick={() => enqueueCompile(source.id)}>{t(compileState(source.id) === 'compiling' ? 'Compiling...' : compileState(source.id) === 'queued' ? 'Queued' : 'Compile Source')}</button>}<button className="danger-command" disabled={remove.isPending} onClick={() => { if (window.confirm(t('Delete this source and every page compiled only from it? This cannot be undone.'))) remove.mutate(source.id) }}>{t(remove.isPending ? 'Deleting...' : 'Delete source')}</button></div></div>
        {remove.isError && <div className="notice error"><AlertCircle size={16} /><span>{remove.error.message}</span><button onClick={() => remove.reset()}>{t('Dismiss')}</button></div>}
        {source.status === 'failed' && <div className="notice error"><AlertCircle size={16} />{t('Parser failed. The original Source remains preserved.')}</div>}
        {compileEntries[source.id]?.state === 'error' && <div className="notice error"><span>{compileEntries[source.id]?.error}</span><button onClick={() => enqueueCompile(source.id)}>{t('Retry')}</button></div>}
        {compileEntries[source.id]?.state === 'done' && <div className="notice success"><span>{t('Compilation')} {compileEntries[source.id]?.status}</span><Link to={`/compilation?run=${compileEntries[source.id]?.runId}`}>{t('Open run')}</Link></div>}
        <div className="metrics"><span><strong>{chunks.data?.length ?? trace.data?.chunk_count ?? 0}</strong> {t('chunks')}</span><span><strong>{trace.data?.claim_count ?? 0}</strong> {t('compiled claims')}</span><span><strong>{source.status}</strong> {t('parser status')}</span></div>
        <section className="chunk-list"><div className="panel-title"><span>{t('Source chunks')}</span><small>{t('stable evidence IDs')}</small></div>
          {chunks.isLoading && <State text={t('Loading chunks...')} retry={t('Retry')} />}{chunks.isError && <State text={t('Could not load chunks.')} retry={t('Retry')} action={() => void chunks.refetch()} />}
          {chunks.data?.map(chunk => <button className={chunk.id === selectedChunk ? 'chunk-row active' : 'chunk-row'} key={chunk.id} onClick={() => inspect(chunk.id)}><span>{location(chunk, t)}</span><p>{chunk.text}</p><small>{chunk.id}</small></button>)}
          {!chunks.isLoading && chunks.data?.length === 0 && <State text={t('No chunks were produced for this Source.')} retry={t('Retry')} />}
        </section>
      </> : <State text={t('Select a Source to inspect parsing and evidence.')} retry={t('Retry')} />}</section>
      <aside className="source-inspector"><div className="panel-title"><span>{t('Citation Inspector')}</span></div>{directChunk.isLoading && <State text={t('Locating citation...')} retry={t('Retry')} />}{directChunk.isError && <State text={t('Citation could not be loaded.')} retry={t('Retry')} action={() => void directChunk.refetch()} />}{visibleChunk && <><h2>{directChunk.data?.source.original_name ?? source?.original_name}</h2><p className="source-location">{location(visibleChunk, t)}</p><blockquote>{visibleChunk.text}</blockquote><dl><dt>{t('Chunk ID')}</dt><dd>{visibleChunk.id}</dd><dt>{t('Source ID')}</dt><dd>{visibleChunk.document_id}</dd><dt>{t('Index')}</dt><dd>{visibleChunk.chunk_index}</dd></dl></>}{!selectedChunk && <State text={t('Choose a chunk or open a citation to inspect its exact source location.')} retry={t('Retry')} />}</aside>
    </div>
  </main>
}

function Status({ value }: { value: string }) { return <span className={`status status-${value}`}>{value}</span> }
function State({ text, retry, action }: { text: string; retry: string; action?: () => void }) { return <div className="inline-state"><span>{text}</span>{action && <button onClick={action}><RefreshCw size={14} />{retry}</button>}</div> }
