import { Activity, BookOpen, Boxes, FileArchive, FileCode2, MessageCircle, Search as SearchIcon, Settings as SettingsIcon, Sparkles } from 'lucide-react'
import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ChatPanel } from './features/chat/ChatPanel'
import { CompilationPanel } from './features/compilation/CompilationPanel'
import { QueryPanel } from './features/query/QueryPanel'
import { SearchPanel } from './features/search/SearchPanel'
import { SettingsPanel } from './features/settings/SettingsPanel'
import { SourcesPanel } from './features/sources/SourcesPanel'
import { WikiBrowser } from './features/wiki/WikiBrowser'
import { getHealth } from './api/contracts'
import './styles.css'

const navigation = [
  { to: '/wiki', label: 'Wiki', icon: BookOpen },
  { to: '/sources', label: 'Sources', icon: FileArchive },
  { to: '/compilation', label: 'Compilation', icon: Boxes },
  { to: '/search', label: 'Search', icon: SearchIcon },
  { to: '/query', label: 'Query', icon: Sparkles },
  { to: '/chat', label: 'Chat', icon: MessageCircle },
  { to: '/settings', label: 'Settings', icon: SettingsIcon },
]

function Layout({ children }: { children: React.ReactNode }) {
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth, refetchInterval: 30000 })
  return <div className="app-shell"><aside className="main-sidebar"><div className="brand"><FileCode2 size={19} /><span>LLM-Wiki</span><small>DEMO</small></div><nav aria-label="Main navigation">{navigation.map(item => { const Icon = item.icon; return <NavLink to={item.to} className={({ isActive }) => isActive ? 'nav-item active' : 'nav-item'} key={item.to}><Icon size={17} /><span>{item.label}</span></NavLink> })}</nav><div className="sidebar-foot"><span><Activity size={14} />API {health.isError ? 'offline' : health.data?.status ?? 'checking'}</span><small>Phase 6 Demo</small></div></aside><div className="app-content"><header className="mobile-bar"><div className="brand"><FileCode2 size={18} /><span>LLM-Wiki</span></div><span className="phase">Demo workspace</span></header>{children}</div></div>
}

export function App() {
  return <Layout><Routes><Route path="/wiki/:slug?" element={<WikiBrowser />} /><Route path="/sources" element={<SourcesPanel />} /><Route path="/compilation" element={<CompilationPanel />} /><Route path="/search" element={<SearchPanel />} /><Route path="/query" element={<QueryPanel />} /><Route path="/chat" element={<ChatPanel />} /><Route path="/settings" element={<SettingsPanel />} /><Route path="/" element={<Navigate to="/wiki" replace />} /><Route path="*" element={<Navigate to="/wiki" replace />} /></Routes></Layout>
}
