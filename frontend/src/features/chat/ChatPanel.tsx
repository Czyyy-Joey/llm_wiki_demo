import { ExternalLink, MessageCircle, RefreshCw, Send } from 'lucide-react'
import { useEffect, useState } from 'react'
import { ConversationDetail, createConversation, getConversation, streamMessage } from '../../api/contracts'
import { useI18n } from '../../i18n'
import { useWorkspace } from '../../workspace'

export function ChatPanel() {
  const { t } = useI18n()
  const [detail, setDetail] = useState<ConversationDetail | null>(null)
  const [restoring, setRestoring] = useState(true)
  const [question, setQuestion] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [streamedAnswer, setStreamedAnswer] = useState('')
  const workspace = useWorkspace()
  const conversationKey = `llm-wiki-conversation:${workspace.selectedID}`

  useEffect(() => {
    setDetail(null)
    setStreamedAnswer('')
    const id = window.localStorage.getItem(conversationKey)
    if (!id) {
      setRestoring(false)
      return
    }
    void getConversation(id)
      .then(setDetail)
      .catch(() => window.localStorage.removeItem(conversationKey))
      .finally(() => setRestoring(false))
  }, [conversationKey])

  async function ensureConversation() {
    if (detail) return detail
    const created = await createConversation('Wiki research')
    window.localStorage.setItem(conversationKey, created.id)
    const next = { conversation: created, messages: [] }
    setDetail(next)
    return next
  }

  async function sendCurrent() {
    if (!question.trim() || busy) return
    setBusy(true); setError(''); setStreamedAnswer('')
    try {
      const conversation = await ensureConversation()
      const result = await streamMessage(conversation.conversation.id, question.trim(), delta => setStreamedAnswer(value => value + delta))
      setDetail({ ...conversation, messages: [...conversation.messages, result.user_message, result.assistant_message] })
      setQuestion(''); setStreamedAnswer('')
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('Request failed')); setStreamedAnswer('') } finally { setBusy(false) }
  }

  function submit(event: React.FormEvent) {
    event.preventDefault()
    void sendCurrent()
  }

  return <main className="work-page chat-page"><header className="page-head"><div><p className="eyebrow">{t('Persistent conversation')}</p><h1>{t('Ask the compiled Wiki')}</h1><p className="muted">{t('Recent history is rewritten into a standalone query; every turn preserves its trace and citations.')}</p></div><MessageCircle size={21} /></header>{restoring && <p className="inline-state">{t('Restoring conversation...')}</p>}<section className="chat-log">{detail?.messages.map(message => <article className={`chat-message ${message.role}`} key={message.id}><span className="chat-role">{t(message.role === 'user' ? 'You' : 'Wiki answer')}</span><p>{message.content}</p>{message.standalone_query && <small className="standalone">{t('Retrieved as:')} {message.standalone_query}</small>}{message.role === 'assistant' && <div className="message-observability"><span>{message.context?.length ?? 0} {t('context items')}</span><span>{t('Trace')} {message.retrieval_trace_id || t('not recorded')}</span></div>}{message.citations?.map(citation => <a className="chat-citation" href={`/sources?chunk=${encodeURIComponent(citation.source_chunk_id || citation.id)}`} key={citation.id}><ExternalLink size={13} />[{citation.label || citation.id}]</a>)}{message.role === 'assistant' && message.context?.filter(item => item.kind === 'wiki_page' || item.kind === 'wiki').map(item => <a className="wiki-reference" href={item.page_id ? `/wiki/${item.page_id}` : '/wiki'} key={`${message.id}-${item.id}`}>{t('Wiki:')} {item.label || item.page_id}</a>)}</article>)}{streamedAnswer && <article className="chat-message assistant streaming"><span className="chat-role">{t('Wiki answer · streaming')}</span><p>{streamedAnswer}</p></article>}{!restoring && !detail && !streamedAnswer && <p className="empty-state">{t('Ask a question to start a persisted conversation.')}</p>}{detail && !detail.messages.length && !streamedAnswer && <p className="empty-state">{t('Ask a question to start a persisted conversation.')}</p>}</section><form className="chat-form" onSubmit={submit}><input value={question} onChange={event => setQuestion(event.target.value)} placeholder={t('Ask a question about the Wiki')} aria-label={t('Ask a question about the Wiki')} /><button type="submit" disabled={busy || !question.trim()} aria-label={t('Send')}><Send size={16} />{t(busy ? 'Working' : 'Send')}</button></form>{error && <div className="notice error"><span>{error}</span><button onClick={() => { setError(''); void sendCurrent() }}><RefreshCw size={14} />{t('Retry')}</button></div>}</main>
}
