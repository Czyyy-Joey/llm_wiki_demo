import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, CheckCircle2, FileText, RefreshCw, Upload } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { compileSource, deleteSource, getSourceChunk, getSourceChunks, getSources, getSourceWiki, uploadSource, type SourceChunk } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

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
  const sources = useQuery({ queryKey: ['sources', workspace.selectedID], queryFn: getSources })
  const chunks = useQuery({ queryKey: ['source-chunks', workspace.selectedID, selectedID], queryFn: () => getSourceChunks(selectedID), enabled: Boolean(selectedID) })
  const trace = useQuery({ queryKey: ['source-wiki', workspace.selectedID, selectedID], queryFn: () => getSourceWiki(selectedID), enabled: Boolean(selectedID) })
  const directChunk = useQuery({ queryKey: ['source-chunk', workspace.selectedID, selectedChunk], queryFn: () => getSourceChunk(selectedChunk), enabled: Boolean(selectedChunk) })
  const upload = useMutation({ mutationFn: uploadSource, onSuccess: value => { void client.invalidateQueries({ queryKey: ['sources', workspace.selectedID] }); setSelectedID(value.source.id); setParams({ source: value.source.id }) } })
  const compile = useMutation({ mutationFn: compileSource })
  const remove = useMutation({
    mutationFn: deleteSource,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['sources', workspace.selectedID] })
      void client.invalidateQueries({ queryKey: ['wiki-pages'] })
      setSelectedID('')
      setSelectedChunk('')
      setParams({})
    },
  })

  useEffect(() => {
    setSelectedID('')
    setSelectedChunk('')
  }, [workspace.selectedID])

  useEffect(() => {
    if (!selectedID && sources.data?.[0]) setSelectedID(sources.data[0].id)
  }, [selectedID, sources.data])

  const source = sources.data?.find(item => item.id === selectedID) ?? directChunk.data?.source
  const visibleChunk = directChunk.data?.chunk ?? chunks.data?.find(item => item.id === selectedChunk)
  function selectSource(id: string) { setSelectedID(id); setSelectedChunk(''); setParams({ source: id }) }
  function inspect(id: string) { setSelectedChunk(id); setParams(selectedID ? { source: selectedID, chunk: id } : { chunk: id }) }

  return <main className="work-page sources-page">
    <header className="page-head"><div><p className="eyebrow">{t('Evidence layer')}</p><h1>{t('Sources')}</h1><p className="muted">{t('Preserved originals, parser status, stable chunks, and compilation entry points.')}</p></div>
      <label className={`upload-button ${upload.isPending ? 'disabled' : ''}`}><Upload size={16} />{t(upload.isPending ? 'Parsing...' : 'Upload source')}<input type="file" accept=".md,.txt,.pdf,text/plain,text/markdown,application/pdf" disabled={upload.isPending} onChange={event => { const file = event.target.files?.[0]; if (file) upload.mutate(file); event.target.value = '' }} /></label>
    </header>
    {upload.isError && <div className="notice error"><AlertCircle size={16} /><span>{upload.error.message}</span><button onClick={() => upload.reset()}>{t('Dismiss')}</button></div>}
    {upload.data && <div className="notice success"><CheckCircle2 size={16} /><span>{upload.data.result.action === 'NO_OP' ? t('Duplicate detected. Existing Source reused.') : `${upload.data.chunks.length} ${t('stable chunks parsed.')}`}</span></div>}
    <div className="split-workspace">
      <aside className="record-list"><div className="panel-title"><span>{t('Source library')}</span><small>{sources.data?.length ?? 0}</small></div>
        {sources.isLoading && <State text={t('Loading Sources...')} retry={t('Retry')} />}
        {sources.isError && <State text={t('Could not load Sources.')} retry={t('Retry')} action={() => void sources.refetch()} />}
        {sources.data?.map(item => <button className={item.id === selectedID ? 'record active' : 'record'} key={item.id} onClick={() => selectSource(item.id)}><FileText size={16} /><span><strong>{item.original_name}</strong><small>{item.media_type}</small></span><Status value={item.status} /></button>)}
        {!sources.isLoading && sources.data?.length === 0 && <State text={t('Upload Markdown, TXT, or a text PDF to begin.')} retry={t('Retry')} />}
      </aside>
      <section className="record-detail">{source ? <>
        <div className="detail-head"><div><Status value={source.status} /><h2>{source.original_name}</h2><p>{source.id}</p></div><div className="detail-actions">{source.status === 'parsed' && <button className="primary-command" disabled={compile.isPending} onClick={() => compile.mutate(source.id)}>{t(compile.isPending ? 'Compiling...' : 'Compile Source')}</button>}<button className="danger-command" disabled={remove.isPending} onClick={() => { if (window.confirm(t('Delete this source and every page compiled only from it? This cannot be undone.'))) remove.mutate(source.id) }}>{t(remove.isPending ? 'Deleting...' : 'Delete source')}</button></div></div>
        {remove.isError && <div className="notice error"><AlertCircle size={16} /><span>{remove.error.message}</span><button onClick={() => remove.reset()}>{t('Dismiss')}</button></div>}
        {source.status === 'failed' && <div className="notice error"><AlertCircle size={16} />{t('Parser failed. The original Source remains preserved.')}</div>}
        {compile.isError && <div className="notice error"><span>{compile.error.message}</span><button onClick={() => compile.mutate(source.id)}>{t('Retry')}</button></div>}
        {compile.data && <div className="notice success"><span>{t('Compilation')} {compile.data.status}</span><Link to={`/compilation?run=${compile.data.run_id}`}>{t('Open run')}</Link></div>}
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
