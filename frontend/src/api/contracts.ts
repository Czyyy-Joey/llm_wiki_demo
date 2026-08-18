export type ProviderStatus = { configured: boolean; endpoint_configured: boolean; model?: string }
export type HealthResponse = { status: string; database: string; providers: { compiler_llm: ProviderStatus; embedding: ProviderStatus } }

export async function getHealth(): Promise<HealthResponse> {
  const response = await fetch('/api/health')
  if (!response.ok) throw new Error('Health check failed')
  return await response.json() as HealthResponse
}
