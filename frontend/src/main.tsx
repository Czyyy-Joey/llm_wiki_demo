import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router-dom'
import { App } from './App'
import { I18nProvider } from './i18n'
import { WorkspaceProvider } from './workspace'

createRoot(document.getElementById('root')!).render(<StrictMode><QueryClientProvider client={new QueryClient()}><I18nProvider><WorkspaceProvider><BrowserRouter><App /></BrowserRouter></WorkspaceProvider></I18nProvider></QueryClientProvider></StrictMode>)
