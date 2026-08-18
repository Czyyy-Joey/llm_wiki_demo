import { useQuery } from '@tanstack/react-query'
import { Database, FileCode2, Settings2 } from 'lucide-react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { getHealth } from './api/contracts'
import './styles.css'

function StatusPage() {
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth })
  const providers = health.data?.providers
  return <main className="shell">
    <header><div className="brand"><FileCode2 size={20} /><span>LLM-Wiki</span></div><span className="phase">Phase 0 / Foundation</span></header>
    <section className="intro"><p className="eyebrow">Knowledge compilation system</p><h1>工程骨架已就绪</h1><p className="muted">当前版本固定全栈边界与核心数据契约，业务模块将在后续阶段逐步接入。</p></section>
    <section className="status-grid" aria-label="System status">
      <article><Database size={18} /><div><h2>Application</h2><strong>{health.isLoading ? '检查中' : health.isError ? '不可用' : health.data?.status ?? '未知'}</strong><p>Go HTTP server</p></div></article>
      <article><Settings2 size={18} /><div><h2>Providers</h2><strong>{providers?.compiler_llm.configured && providers.embedding.configured ? 'Ready' : '待配置'}</strong><p>Compiler / Embedding</p></div></article>
    </section>
    <footer>API contract and OpenAPI are available from the backend.</footer>
  </main>
}

export function App() {
  return <Routes><Route path="/" element={<StatusPage />} /><Route path="*" element={<Navigate to="/" replace />} /></Routes>
}
