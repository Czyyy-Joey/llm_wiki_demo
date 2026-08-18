import { BookOpen, FileArchive, FileCode2, Search as SearchIcon } from 'lucide-react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { WikiBrowser } from './features/wiki/WikiBrowser'
import { SearchPanel } from './features/search/SearchPanel'
import './styles.css'

function Layout({ search = false }: { search?: boolean }) {
  return <div className="app-shell"><header className="app-bar"><div className="brand"><FileCode2 size={19} /><span>LLM-Wiki</span></div><nav><a className={!search ? 'active' : ''} href="/wiki"><BookOpen size={16} /><span>Wiki</span></a><a className={search ? 'active' : ''} href="/search" aria-label="Search"><SearchIcon size={16} /><span>Search</span></a><span><FileArchive size={16} />Sources</span></nav><span className="phase">Phase 4</span></header>{search ? <SearchPanel /> : <WikiBrowser />}</div>
}

export function App() {
  return <Routes><Route path="/wiki/:slug?" element={<Layout />} /><Route path="/search" element={<Layout search />} /><Route path="/" element={<Navigate to="/wiki" replace />} /><Route path="*" element={<Navigate to="/wiki" replace />} /></Routes>
}
