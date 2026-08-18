import { describe, expect, it, vi } from 'vitest'
import { getHealth } from './contracts'

describe('API contracts', () => { it('reads the backend response body directly', async () => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ status: 'ok', database: 'ok', providers: { compiler_llm: { configured: false, endpoint_configured: false }, embedding: { configured: false, endpoint_configured: false } } }) })); await expect(getHealth()).resolves.toMatchObject({ status: 'ok', database: 'ok' }); vi.unstubAllGlobals() }) })
