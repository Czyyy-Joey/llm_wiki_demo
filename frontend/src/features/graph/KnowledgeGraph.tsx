import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { ArrowRight, Focus, Share2, X } from 'lucide-react'
import { getWikiGraph, type GraphNode, type PageType } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

type SimNode = GraphNode & { x: number; y: number; vx: number; vy: number; pinned: boolean }
type Transform = { x: number; y: number; k: number }

const WORLD_WIDTH = 960
const WORLD_HEIGHT = 680
const typeColor: Record<PageType, string> = { concept: '#2e6f81', entity: '#816f2e', topic: '#75558a' }
const typeLabel: Record<PageType, string> = { concept: 'Concepts', entity: 'Entities', topic: 'Topics' }

function nodeRadius(node: GraphNode): number {
  return 9 + Math.min(14, node.link_count * 2)
}

// initialCircle seeds a deterministic layout so the simulation settles the same
// way each mount instead of flickering from random starts.
function initialCircle(nodes: GraphNode[]): SimNode[] {
  const radius = Math.min(WORLD_WIDTH, WORLD_HEIGHT) / 2 - 80
  return nodes.map((node, index) => {
    const angle = (index / Math.max(1, nodes.length)) * Math.PI * 2
    return {
      ...node,
      x: WORLD_WIDTH / 2 + Math.cos(angle) * radius,
      y: WORLD_HEIGHT / 2 + Math.sin(angle) * radius,
      vx: 0,
      vy: 0,
      pinned: false,
    }
  })
}

// simulate advances one force-directed tick in place: node repulsion, spring
// attraction along edges, and a gentle pull to the center. alpha cools the
// motion so the layout settles instead of vibrating forever.
function simulate(nodes: SimNode[], edges: { source: number; target: number }[], alpha: number) {
  const repulsion = 5200
  const springLength = 130
  const springStrength = 0.05
  const gravity = 0.018
  for (let i = 0; i < nodes.length; i++) {
    for (let j = i + 1; j < nodes.length; j++) {
      const a = nodes[i]
      const b = nodes[j]
      let dx = a.x - b.x
      let dy = a.y - b.y
      let distSq = dx * dx + dy * dy
      if (distSq < 0.01) {
        dx = (Math.random() - 0.5) * 2
        dy = (Math.random() - 0.5) * 2
        distSq = dx * dx + dy * dy
      }
      const dist = Math.sqrt(distSq)
      const force = (repulsion / distSq) * alpha
      const fx = (dx / dist) * force
      const fy = (dy / dist) * force
      a.vx += fx
      a.vy += fy
      b.vx -= fx
      b.vy -= fy
    }
  }
  for (const edge of edges) {
    const a = nodes[edge.source]
    const b = nodes[edge.target]
    if (!a || !b) continue
    const dx = b.x - a.x
    const dy = b.y - a.y
    const dist = Math.sqrt(dx * dx + dy * dy) || 1
    const force = (dist - springLength) * springStrength * alpha
    const fx = (dx / dist) * force
    const fy = (dy / dist) * force
    a.vx += fx
    a.vy += fy
    b.vx -= fx
    b.vy -= fy
  }
  for (const node of nodes) {
    node.vx += (WORLD_WIDTH / 2 - node.x) * gravity * alpha
    node.vy += (WORLD_HEIGHT / 2 - node.y) * gravity * alpha
    if (node.pinned) {
      node.vx = 0
      node.vy = 0
      continue
    }
    node.vx *= 0.82
    node.vy *= 0.82
    node.x += node.vx
    node.y += node.vy
  }
}

export function KnowledgeGraph() {
  const { t } = useI18n()
  const workspace = useWorkspace()
  const graph = useQuery({ queryKey: ['wiki-graph', workspace.selectedID], queryFn: getWikiGraph })
  const svgRef = useRef<SVGSVGElement | null>(null)
  const nodesRef = useRef<SimNode[]>([])
  const frameRef = useRef<number>(0)
  const [, forceRender] = useState(0)
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [transform, setTransform] = useState<Transform>({ x: 0, y: 0, k: 1 })
  const drag = useRef<{ kind: 'node' | 'canvas'; id?: string; startX: number; startY: number; originX: number; originY: number } | null>(null)

  const edges = useMemo(() => graph.data?.edges ?? [], [graph.data])
  const nodeIndex = useMemo(() => {
    const map = new Map<string, number>()
    ;(graph.data?.nodes ?? []).forEach((node, index) => map.set(node.id, index))
    return map
  }, [graph.data])
  const edgePairs = useMemo(
    () =>
      edges
        .map(edge => ({ source: nodeIndex.get(edge.source) ?? -1, target: nodeIndex.get(edge.target) ?? -1 }))
        .filter(pair => pair.source >= 0 && pair.target >= 0),
    [edges, nodeIndex],
  )

  const neighbors = useMemo(() => {
    const map = new Map<string, Set<string>>()
    for (const edge of edges) {
      if (!map.has(edge.source)) map.set(edge.source, new Set())
      if (!map.has(edge.target)) map.set(edge.target, new Set())
      map.get(edge.source)!.add(edge.target)
      map.get(edge.target)!.add(edge.source)
    }
    return map
  }, [edges])

  // Rebuild the simulation whenever the node set changes, then run the
  // force layout on requestAnimationFrame until it cools below threshold.
  useEffect(() => {
    const nodes = graph.data?.nodes ?? []
    nodesRef.current = initialCircle(nodes)
    setSelectedID(current => (current && nodeIndex.has(current) ? current : null))
    if (nodes.length === 0) {
      forceRender(value => value + 1)
      return
    }
    let alpha = 1
    const run = () => {
      alpha *= 0.985
      const iterations = alpha > 0.5 ? 3 : 1
      for (let i = 0; i < iterations; i++) simulate(nodesRef.current, edgePairs, alpha)
      forceRender(value => value + 1)
      if (alpha > 0.02) frameRef.current = requestAnimationFrame(run)
    }
    frameRef.current = requestAnimationFrame(run)
    return () => cancelAnimationFrame(frameRef.current)
  }, [graph.data, edgePairs, nodeIndex])

  const toWorld = (clientX: number, clientY: number) => {
    const rect = svgRef.current?.getBoundingClientRect()
    if (!rect) return { x: 0, y: 0 }
    const scaleX = WORLD_WIDTH / rect.width
    const scaleY = WORLD_HEIGHT / rect.height
    return {
      x: ((clientX - rect.left) * scaleX - transform.x) / transform.k,
      y: ((clientY - rect.top) * scaleY - transform.y) / transform.k,
    }
  }

  const onPointerDownNode = (event: React.PointerEvent, node: SimNode) => {
    event.stopPropagation()
    ;(event.target as Element).setPointerCapture(event.pointerId)
    setSelectedID(node.id)
    const world = toWorld(event.clientX, event.clientY)
    node.pinned = true
    drag.current = { kind: 'node', id: node.id, startX: world.x, startY: world.y, originX: node.x, originY: node.y }
  }

  const onPointerDownCanvas = (event: React.PointerEvent) => {
    ;(event.currentTarget as Element).setPointerCapture(event.pointerId)
    drag.current = { kind: 'canvas', startX: event.clientX, startY: event.clientY, originX: transform.x, originY: transform.y }
  }

  const onPointerMove = (event: React.PointerEvent) => {
    const state = drag.current
    if (!state) return
    if (state.kind === 'node') {
      const node = nodesRef.current.find(item => item.id === state.id)
      if (!node) return
      const world = toWorld(event.clientX, event.clientY)
      node.x = state.originX + (world.x - state.startX)
      node.y = state.originY + (world.y - state.startY)
      node.vx = 0
      node.vy = 0
      forceRender(value => value + 1)
    } else {
      const rect = svgRef.current?.getBoundingClientRect()
      const scaleX = rect ? WORLD_WIDTH / rect.width : 1
      const scaleY = rect ? WORLD_HEIGHT / rect.height : 1
      setTransform(current => ({ ...current, x: state.originX + (event.clientX - state.startX) * scaleX, y: state.originY + (event.clientY - state.startY) * scaleY }))
    }
  }

  const onPointerUp = () => {
    if (drag.current?.kind === 'node') {
      const node = nodesRef.current.find(item => item.id === drag.current?.id)
      if (node) node.pinned = false
    }
    drag.current = null
  }

  const onWheel = (event: React.WheelEvent) => {
    const world = toWorld(event.clientX, event.clientY)
    const factor = event.deltaY < 0 ? 1.12 : 1 / 1.12
    setTransform(current => {
      const k = Math.min(3, Math.max(0.35, current.k * factor))
      return { k, x: current.x + world.x * (current.k - k), y: current.y + world.y * (current.k - k) }
    })
  }

  const resetView = () => setTransform({ x: 0, y: 0, k: 1 })

  const nodes = nodesRef.current
  const selected = graph.data?.nodes.find(node => node.id === selectedID) ?? null
  const activeNeighbors = selectedID ? neighbors.get(selectedID) ?? new Set<string>() : new Set<string>()
  const isDimmed = (id: string) => Boolean(selectedID) && id !== selectedID && !activeNeighbors.has(id)

  return <div className="graph-workspace">
    <div className="graph-stage" onWheel={onWheel}>
      <div className="graph-toolbar">
        <div className="graph-title"><Share2 size={17} /><span>{t('Knowledge Graph')}</span></div>
        <div className="graph-legend">
          {(Object.keys(typeColor) as PageType[]).map(type => <span key={type}><i style={{ background: typeColor[type] }} />{t(typeLabel[type])}</span>)}
        </div>
        <button className="icon-button" title={t('Reset view')} onClick={resetView}><Focus size={17} /></button>
      </div>
      {graph.isLoading && <p className="graph-state">{t('Building the knowledge graph...')}</p>}
      {graph.isError && <p className="graph-state">{t('Graph API is unavailable.')}</p>}
      {graph.data && graph.data.nodes.length === 0 && <p className="graph-state">{t('No compiled pages to graph yet.')}</p>}
      {graph.data && graph.data.nodes.length > 0 && <svg
        ref={svgRef}
        className="graph-canvas"
        viewBox={`0 0 ${WORLD_WIDTH} ${WORLD_HEIGHT}`}
        preserveAspectRatio="xMidYMid meet"
        onPointerDown={onPointerDownCanvas}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerLeave={onPointerUp}
        role="application"
        aria-label={t('Knowledge Graph')}
      >
        <defs>
          <marker id="graph-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="#9fb0b6" />
          </marker>
        </defs>
        <g transform={`translate(${transform.x} ${transform.y}) scale(${transform.k})`}>
          {edgePairs.map((pair, index) => {
            const a = nodes[pair.source]
            const b = nodes[pair.target]
            if (!a || !b) return null
            const highlighted = selectedID === a.id || selectedID === b.id
            const dimmed = Boolean(selectedID) && !highlighted
            return <line
              key={`${a.id}-${b.id}-${index}`}
              className={`graph-edge${highlighted ? ' highlighted' : ''}${dimmed ? ' dimmed' : ''}`}
              x1={a.x}
              y1={a.y}
              x2={b.x}
              y2={b.y}
              markerEnd="url(#graph-arrow)"
            />
          })}
          {nodes.map(node => {
            const radius = nodeRadius(node)
            const dimmed = isDimmed(node.id)
            return <g
              key={node.id}
              className={`graph-node${node.id === selectedID ? ' selected' : ''}${dimmed ? ' dimmed' : ''}`}
              transform={`translate(${node.x} ${node.y})`}
              onPointerDown={event => onPointerDownNode(event, node)}
            >
              <circle r={radius} fill={typeColor[node.page_type]} />
              <text y={radius + 14} textAnchor="middle">{node.title}</text>
            </g>
          })}
        </g>
      </svg>}
    </div>

    <aside className="graph-inspector" aria-label={t('Document details')}>
      {selected ? <>
        <div className="graph-inspector-head">
          <span className={`type-mark ${selected.page_type}`}>{selected.page_type}</span>
          <button className="icon-button" title={t('Clear selection')} onClick={() => setSelectedID(null)}><X size={16} /></button>
        </div>
        <h2>{selected.title}</h2>
        <p className="graph-inspector-summary">{selected.summary || t('No summary available.')}</p>
        <dl className="graph-inspector-stats">
          <div><dt>{t('claims')}</dt><dd>{selected.claim_count}</dd></div>
          <div><dt>{t('sources')}</dt><dd>{selected.source_count}</dd></div>
          <div><dt>{t('Connections')}</dt><dd>{activeNeighbors.size}</dd></div>
        </dl>
        {activeNeighbors.size > 0 && <div className="graph-inspector-neighbors">
          <h3>{t('Connected pages')}</h3>
          {[...activeNeighbors].map(id => {
            const neighbor = graph.data?.nodes.find(node => node.id === id)
            if (!neighbor) return null
            return <button key={id} className="graph-neighbor" onClick={() => setSelectedID(id)}>{neighbor.title}</button>
          })}
        </div>}
        <Link className="graph-open-link" to={`/wiki/${selected.slug}`}>{t('Open full page')}<ArrowRight size={15} /></Link>
      </> : <div className="graph-inspector-empty">
        <Share2 size={20} />
        <p>{t('Select a node to inspect the document and its links.')}</p>
        <span>{t('Drag nodes to rearrange · scroll to zoom · drag the background to pan.')}</span>
      </div>}
    </aside>
  </div>
}
