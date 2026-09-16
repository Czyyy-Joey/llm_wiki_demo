const knowledgeBaseStorageKey = 'llm-wiki-knowledge-base'

export function getSelectedKnowledgeBaseID(): string {
  return window.localStorage.getItem(knowledgeBaseStorageKey) || 'default'
}

export function setSelectedKnowledgeBaseID(id: string): void {
  window.localStorage.setItem(knowledgeBaseStorageKey, id)
}
