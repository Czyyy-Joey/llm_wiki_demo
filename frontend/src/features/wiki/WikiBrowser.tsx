import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BookOpen, ChevronRight, FileText, GitBranch, History, Link2, PanelRightClose, PanelRightOpen } from 'lucide-react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { getWikiPage, getWikiPages, getWikiRevisions, type EvidenceView, type PageType } from '../../api/contracts'

const types: { value: PageType; label: string }[] = [
  { value: 'concept', label: 'Concepts' },
  { value: 'entity', label: 'Entities' },
  { value: 'topic', label: 'Topics' },
]

function locationLabel(evidence: EvidenceView) {
  const parts = []
  if (evidence.chunk.page_number) parts.push(`Page ${evidence.chunk.page_number}`)
  if (evidence.chunk.heading_path?.length) parts.push(evidence.chunk.heading_path.join(' / '))
  parts.push(`Chars ${evidence.chunk.char_start}-${evidence.chunk.char_end}`)
  return parts.join(' · ')
}

export function WikiBrowser() {
  const { slug } = useParams()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const pages = useQuery({ queryKey: ['wiki-pages'], queryFn: () => getWikiPages() })
  const detail = useQuery({ queryKey: ['wiki-page', slug], queryFn: () => getWikiPage(slug!), enabled: Boolean(slug) })
  const revisions = useQuery({ queryKey: ['wiki-revisions', slug], queryFn: () => getWikiRevisions(slug!), enabled: Boolean(slug) })
  const [selectedEvidence, setSelectedEvidence] = useState<EvidenceView | null>(null)
  const inspectorRef = useRef<HTMLElement | null>(null)
  const [inspectorOpen, setInspectorOpen] = useState(() => !window.matchMedia('(max-width: 760px)').matches)
  const [view, setView] = useState<'knowledge' | 'revisions'>('knowledge')

  useEffect(() => {
    if (!slug && pages.data?.[0]) navigate(`/wiki/${pages.data[0].slug}`, { replace: true })
  }, [navigate, pages.data, slug])

  useEffect(() => {
    const citationID = searchParams.get('citation')
    if (!citationID || !detail.data) return
    const evidence = detail.data.claims.flatMap(item => item.evidence).find(item => item.chunk.id === citationID)
    if (evidence) {
      setSelectedEvidence(evidence)
      setInspectorOpen(true)
    }
  }, [detail.data, searchParams])

  useEffect(() => {
    if (selectedEvidence && inspectorOpen) inspectorRef.current?.scrollIntoView({ block: 'nearest' })
  }, [inspectorOpen, selectedEvidence])

  const grouped = useMemo(() => types.map(type => ({ ...type, pages: pages.data?.filter(page => page.page_type === type.value) ?? [] })), [pages.data])

  return <div className={`wiki-workspace ${inspectorOpen ? '' : 'inspector-collapsed'}`}>
    <aside className="wiki-tree" aria-label="Wiki pages">
      <div className="tree-title"><BookOpen size={17} /><span>Compiled Wiki</span></div>
      {pages.isLoading && <p className="empty-state">Loading pages...</p>}
      {pages.isError && <p className="empty-state">Wiki API is unavailable.</p>}
      {grouped.map(group => <section className="tree-group" key={group.value}>
        <h2>{group.label}<span>{group.pages.length}</span></h2>
        {group.pages.map(page => <Link className={page.slug === slug ? 'tree-link active' : 'tree-link'} to={`/wiki/${page.slug}`} key={page.id}>
          <span>{page.title}</span><small>{page.source_count} src</small>
        </Link>)}
      </section>)}
      {!pages.isLoading && pages.data?.length === 0 && <p className="empty-state">No compiled pages yet.</p>}
    </aside>

    <main className="wiki-reader">
      {detail.isLoading && <div className="reader-state">Loading compiled knowledge...</div>}
      {detail.isError && <div className="reader-state">This Wiki page could not be loaded.</div>}
      {detail.data && detail.data.page.status !== 'active' && <div className="reader-state inactive-page">
        <h1>{detail.data.page.title}</h1>
        <p>This page is {detail.data.page.status} and is no longer part of the active Wiki.</p>
        {detail.data.canonical_page && <Link to={`/wiki/${detail.data.canonical_page.slug}`}>Open canonical page: {detail.data.canonical_page.title}</Link>}
      </div>}
      {detail.data && detail.data.page.status === 'active' && <>
        <header className="reader-header">
          <div><span className={`type-mark ${detail.data.page.page_type}`}>{detail.data.page.page_type}</span><h1>{detail.data.page.title}</h1></div>
          <button className="icon-button" title={inspectorOpen ? 'Close citation inspector' : 'Open citation inspector'} onClick={() => setInspectorOpen(value => !value)}>{inspectorOpen ? <PanelRightClose size={19} /> : <PanelRightOpen size={19} />}</button>
        </header>
        <div className="reader-meta"><span>Revision {detail.data.page.current_revision}</span><span>{detail.data.claims.length} claims</span><span>{detail.data.sources.length} sources</span></div>
        <nav className="view-tabs" aria-label="Page views">
          <button className={view === 'knowledge' ? 'active' : ''} onClick={() => setView('knowledge')}><FileText size={15} />Knowledge</button>
          <button className={view === 'revisions' ? 'active' : ''} onClick={() => setView('revisions')}><History size={15} />Revisions</button>
        </nav>
        {view === 'knowledge' ? <>
          <p className="page-summary">{detail.data.page.summary}</p>
          {detail.data.sections.map(section => <section className="knowledge-section" key={section.section.id}>
            <h2>{section.section.heading}</h2>
            {section.claims.filter(item => item.claim.status !== 'superseded').map(item => <article className={`claim ${item.claim.status}`} key={item.claim.id}>
              <div className="claim-heading"><span>{item.claim.claim_type}</span>{item.claim.status === 'disputed' && <strong>Disputed</strong>}</div>
              <p>{item.claim.text}</p>
              <div className="citations">{item.evidence.map((evidence, index) => <button key={`${evidence.chunk.id}-${evidence.evidence.relation}`} onClick={() => { setSelectedEvidence(evidence); setInspectorOpen(true) }}>
                [{index + 1}] {evidence.source.original_name}<ChevronRight size={13} />
              </button>)}</div>
            </article>)}
          </section>)}
          <section className="relations">
            <h2><Link2 size={17} />Connections</h2>
            <div className="relation-columns"><div><h3>Links</h3>{detail.data.links.map(item => <Link to={`/wiki/${item.page.slug}`} key={`${item.page.id}-${item.link.relation}`}>{item.page.title}<small>{item.link.relation}</small></Link>)}{detail.data.links.length === 0 && <span>None</span>}</div>
            <div><h3>Backlinks</h3>{detail.data.backlinks.map(item => <Link to={`/wiki/${item.page.slug}`} key={`${item.page.id}-${item.link.relation}`}>{item.page.title}<small>{item.link.relation}</small></Link>)}{detail.data.backlinks.length === 0 && <span>None</span>}</div></div>
          </section>
        </> : <section className="revision-list">
          {revisions.data?.map(item => <article key={item.revision.revision_number}>
            <div className="revision-number"><GitBranch size={16} /><strong>Revision {item.revision.revision_number}</strong><time>{new Date(item.revision.created_at).toLocaleString()}</time></div>
            <p>{item.revision.change_summary}</p>
            <div className="diff-row"><span>+{item.diff.added_claims.length} added</span><span>~{item.diff.changed.length} changed</span><span>-{item.diff.removed_claims.length} removed</span></div>
          </article>)}
        </section>}
      </>}
    </main>

    {inspectorOpen && <aside ref={inspectorRef} className="citation-inspector" aria-label="Citation inspector">
      <div className="inspector-title"><FileText size={17} /><span>Source Inspector</span><button className="inspector-close" aria-label="Close citation inspector" title="Close citation inspector" onClick={() => setInspectorOpen(false)}><PanelRightClose size={16} /></button></div>
      {selectedEvidence ? <>
        <h2>{selectedEvidence.source.original_name}</h2>
        <p className="source-location">{locationLabel(selectedEvidence)}</p>
        <blockquote>{selectedEvidence.chunk.text}</blockquote>
        <dl><div><dt>Chunk</dt><dd>{selectedEvidence.chunk.id}</dd></div><div><dt>Relation</dt><dd>{selectedEvidence.evidence.relation}</dd></div><div><dt>Media</dt><dd>{selectedEvidence.source.media_type}</dd></div></dl>
      </> : <div className="inspector-empty"><ChevronRight size={18} /><p>Select a citation to inspect the exact Source Chunk and its original location.</p><Link className="inspector-link" to="/sources">Open Sources</Link></div>}
    </aside>}
  </div>
}
