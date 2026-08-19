import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertCircle, CheckCircle2, FileText, RefreshCw, Upload } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { compileSource, getSourceChunk, getSourceChunks, getSources, getSourceWiki, uploadSource, type SourceChunk } from '../../api/contracts'

function location(chunk: SourceChunk) {
  const parts = chunk.page_number ? [`Page ${chunk.page_number}`] : []
  if (chunk.heading_path?.length) parts.push(chunk.heading_path.join(' / '))
  parts.push(`Chars ${chunk.char_start}-${chunk.char_end}`)
  return parts.join(' · ')
}

export function SourcesPanel() {
  const client = useQueryClient()
  const [params, setParams] = useSearchParams()
  const [selectedID, setSelectedID] = useState(params.get('source') ?? '')
  const [selectedChunk, setSelectedChunk] = useState(params.get('chunk') ?? '')
  const sources = useQuery({ queryKey: ['sources'], queryFn: getSources })
  const chunks = useQuery({ queryKey: ['source-chunks', selectedID], queryFn: () => getSourceChunks(selectedID), enabled: Boolean(selectedID) })
  const trace = useQuery({ queryKey: ['source-wiki', selectedID], queryFn: () => getSourceWiki(selectedID), enabled: Boolean(selectedID) })
  const directChunk = useQuery({ queryKey: ['source-chunk', selectedChunk], queryFn: () => getSourceChunk(selectedChunk), enabled: Boolean(selectedChunk) })
  const upload = useMutation({ mutationFn: uploadSource, onSuccess: value => { void client.invalidateQueries({ queryKey: ['sources'] }); setSelectedID(value.source.id); setParams({ source: value.source.id }) } })
  const compile = useMutation({ mutationFn: compileSource })

  useEffect(() => {
    if (!selectedID && sources.data?.[0]) setSelectedID(sources.data[0].id)
  }, [selectedID, sources.data])

  const source = sources.data?.find(item => item.id === selectedID) ?? directChunk.data?.source
  const visibleChunk = directChunk.data?.chunk ?? chunks.data?.find(item => item.id === selectedChunk)
  function selectSource(id: string) { setSelectedID(id); setSelectedChunk(''); setParams({ source: id }) }
  function inspect(id: string) { setSelectedChunk(id); setParams(selectedID ? { source: selectedID, chunk: id } : { chunk: id }) }

  return <main className="work-page sources-page">
    <header className="page-head"><div><p className="eyebrow">Evidence layer</p><h1>Sources</h1><p className="muted">Preserved originals, parser status, stable chunks, and compilation entry points.</p></div>
      <label className={`upload-button ${upload.isPending ? 'disabled' : ''}`}><Upload size={16} />{upload.isPending ? 'Parsing...' : 'Upload source'}<input type="file" accept=".md,.txt,.pdf,text/plain,text/markdown,application/pdf" disabled={upload.isPending} onChange={event => { const file = event.target.files?.[0]; if (file) upload.mutate(file); event.target.value = '' }} /></label>
    </header>
    {upload.isError && <div className="notice error"><AlertCircle size={16} /><span>{upload.error.message}</span><button onClick={() => upload.reset()}>Dismiss</button></div>}
    {upload.data && <div className="notice success"><CheckCircle2 size={16} /><span>{upload.data.result.action === 'NO_OP' ? 'Duplicate detected. Existing Source reused.' : `${upload.data.chunks.length} stable chunks parsed.`}</span></div>}
    <div className="split-workspace">
      <aside className="record-list"><div className="panel-title"><span>Source library</span><small>{sources.data?.length ?? 0}</small></div>
        {sources.isLoading && <State text="Loading Sources..." />}
        {sources.isError && <State text="Could not load Sources." action={() => void sources.refetch()} />}
        {sources.data?.map(item => <button className={item.id === selectedID ? 'record active' : 'record'} key={item.id} onClick={() => selectSource(item.id)}><FileText size={16} /><span><strong>{item.original_name}</strong><small>{item.media_type}</small></span><Status value={item.status} /></button>)}
        {!sources.isLoading && sources.data?.length === 0 && <State text="Upload Markdown, TXT, or a text PDF to begin." />}
      </aside>
      <section className="record-detail">{source ? <>
        <div className="detail-head"><div><Status value={source.status} /><h2>{source.original_name}</h2><p>{source.id}</p></div>{source.status === 'parsed' && <button className="primary-command" disabled={compile.isPending} onClick={() => compile.mutate(source.id)}>{compile.isPending ? 'Compiling...' : 'Compile Source'}</button>}</div>
        {source.status === 'failed' && <div className="notice error"><AlertCircle size={16} />Parser failed. The original Source remains preserved.</div>}
        {compile.isError && <div className="notice error"><span>{compile.error.message}</span><button onClick={() => compile.mutate(source.id)}>Retry</button></div>}
        {compile.data && <div className="notice success"><span>Compilation {compile.data.status}</span><Link to={`/compilation?run=${compile.data.run_id}`}>Open run</Link></div>}
        <div className="metrics"><span><strong>{chunks.data?.length ?? trace.data?.chunk_count ?? 0}</strong> chunks</span><span><strong>{trace.data?.claim_count ?? 0}</strong> compiled claims</span><span><strong>{source.status}</strong> parser status</span></div>
        <section className="chunk-list"><div className="panel-title"><span>Source chunks</span><small>stable evidence IDs</small></div>
          {chunks.isLoading && <State text="Loading chunks..." />}{chunks.isError && <State text="Could not load chunks." action={() => void chunks.refetch()} />}
          {chunks.data?.map(chunk => <button className={chunk.id === selectedChunk ? 'chunk-row active' : 'chunk-row'} key={chunk.id} onClick={() => inspect(chunk.id)}><span>{location(chunk)}</span><p>{chunk.text}</p><small>{chunk.id}</small></button>)}
          {!chunks.isLoading && chunks.data?.length === 0 && <State text="No chunks were produced for this Source." />}
        </section>
      </> : <State text="Select a Source to inspect parsing and evidence." />}</section>
      <aside className="source-inspector"><div className="panel-title"><span>Citation Inspector</span></div>{directChunk.isLoading && <State text="Locating citation..." />}{directChunk.isError && <State text="Citation could not be loaded." action={() => void directChunk.refetch()} />}{visibleChunk && <><h2>{directChunk.data?.source.original_name ?? source?.original_name}</h2><p className="source-location">{location(visibleChunk)}</p><blockquote>{visibleChunk.text}</blockquote><dl><dt>Chunk ID</dt><dd>{visibleChunk.id}</dd><dt>Source ID</dt><dd>{visibleChunk.document_id}</dd><dt>Index</dt><dd>{visibleChunk.chunk_index}</dd></dl></>}{!selectedChunk && <State text="Choose a chunk or open a citation to inspect its exact source location." />}</aside>
    </div>
  </main>
}

function Status({ value }: { value: string }) { return <span className={`status status-${value}`}>{value}</span> }
function State({ text, action }: { text: string; action?: () => void }) { return <div className="inline-state"><span>{text}</span>{action && <button onClick={action}><RefreshCw size={14} />Retry</button>}</div> }
