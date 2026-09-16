import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createKnowledgeBase, getKnowledgeBases, type KnowledgeBase } from './api/contracts'
import { getSelectedKnowledgeBaseID, setSelectedKnowledgeBaseID } from './api/scope'

type WorkspaceValue = { knowledgeBases: KnowledgeBase[]; current?: KnowledgeBase; selectedID: string; select: (id: string) => void; refresh: () => Promise<void>; create: (name: string, description: string, language: 'zh' | 'en') => Promise<KnowledgeBase> }
const WorkspaceContext = createContext<WorkspaceValue | null>(null)

export function WorkspaceProvider({ children }: { children: React.ReactNode }) {
  const client = useQueryClient()
  const bases = useQuery({ queryKey: ['knowledge-bases'], queryFn: getKnowledgeBases })
  const [selectedID, setSelectedID] = useState(getSelectedKnowledgeBaseID)
  const current = bases.data?.find(item => item.id === selectedID)
  useEffect(() => {
    if (!bases.data?.length) return
    if (!bases.data.some(item => item.id === selectedID)) {
      const next = bases.data.find(item => item.id === 'default') ?? bases.data[0]
      setSelectedID(next.id)
      setSelectedKnowledgeBaseID(next.id)
    }
  }, [bases.data, selectedID])
  const select = useCallback((id: string) => {
    const next = bases.data?.find(item => item.id === id && item.status === 'active')
    if (!next) return
    setSelectedID(id)
    setSelectedKnowledgeBaseID(id)
    client.removeQueries({ predicate: query => query.queryKey[0] !== 'knowledge-bases' })
    void client.invalidateQueries()
  }, [bases.data, client])
  const refresh = useCallback(async () => { await client.invalidateQueries({ queryKey: ['knowledge-bases'] }) }, [client])
  const create = useCallback(async (name: string, description: string, language: 'zh' | 'en') => {
    const created = await createKnowledgeBase({ name, description, language })
    await refresh()
    setSelectedID(created.id)
    setSelectedKnowledgeBaseID(created.id)
    client.removeQueries({ predicate: query => query.queryKey[0] !== 'knowledge-bases' })
    await client.invalidateQueries()
    return created
  }, [client, refresh])
  const value = useMemo(() => ({ knowledgeBases: bases.data ?? [], current, selectedID, select, refresh, create }), [bases.data, create, current, refresh, select, selectedID])
  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>
}

export function useWorkspace(): WorkspaceValue {
  const value = useContext(WorkspaceContext)
  if (!value) throw new Error('useWorkspace must be used inside WorkspaceProvider')
  return value
}
