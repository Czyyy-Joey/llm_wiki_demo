import { defineConfig, devices } from '@playwright/test'

const backendPort = 18081
const frontendPort = 4173

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: 'list',
  use: {
    baseURL: `http://127.0.0.1:${frontendPort}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
  webServer: [
    {
      command: `sh -c 'root=$(mktemp -d); trap "rm -rf $root" EXIT; env APP_ADDR=127.0.0.1:${backendPort} DATABASE_URL="file:$root/app.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" DATA_ROOT="$root" INDEX_DIR="$root/indexes" COMPILER_LLM_FAKE_FALLBACK=true CHAT_LLM_FAKE_FALLBACK=true go run ./cmd/server'`,
      cwd: '../backend',
      url: `http://127.0.0.1:${backendPort}/api/health`,
      timeout: 120_000,
      reuseExistingServer: false,
    },
    {
      command: `npm run dev -- --host 127.0.0.1 --port ${frontendPort}`,
      env: { VITE_API_PROXY_TARGET: `http://127.0.0.1:${backendPort}` },
      url: `http://127.0.0.1:${frontendPort}`,
      timeout: 60_000,
      reuseExistingServer: false,
    },
  ],
})
