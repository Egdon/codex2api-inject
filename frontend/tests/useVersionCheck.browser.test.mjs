import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
const hookPath = fileURLToPath(new URL('../src/hooks/useVersionCheck.ts', import.meta.url))
const CACHE_KEY = 'codex2api_latest_release'
const LEGACY_KEY = 'codex2api_latest_version'
let browser
let harnessScript

before(async () => {
  // Compile the real hook and React rather than mirroring their async behavior.
  const result = await build({
    configFile: false,
    root,
    logLevel: 'silent',
    define: { __APP_VERSION__: 'window.__frontendVersion', 'process.env.NODE_ENV': '"production"' },
    plugins: [{
      name: 'version-check-harness',
      enforce: 'pre',
      resolveId(id, importer) {
        if (id === 'virtual:version-check-harness') return '\0version-check-harness'
        if (id === '../api' && importer === hookPath) return '\0version-check-api'
      },
      load(id) {
        if (id === '\0version-check-api') {
          return `export const api = {
            getHealth: options => window.__request('health', options),
            getSystemUpdate: source => window.__request('update', source),
          }`
        }
        if (id !== '\0version-check-harness') return
        return `
          import { createElement } from 'react'
          import { createRoot } from 'react-dom/client'
          import { useVersionCheck } from ${JSON.stringify(hookPath)}
          let root
          function Harness({ triggerKey, source, enabled }) {
            const { refreshVersion, ...state } = useVersionCheck(triggerKey, source, enabled)
            window.__versionState = state
            window.__refresh = refreshVersion
            return createElement('output', null, JSON.stringify(state))
          }
          window.__render = (triggerKey, source = 'patched', enabled = true) => root.render(createElement(Harness, { triggerKey, source, enabled }))
          window.__mount = (triggerKey, source = 'patched', enabled = true) => {
            root = createRoot(document.getElementById('root'))
            window.__render(triggerKey, source, enabled)
          }
          window.__unmount = () => root.unmount()
          window.__mount('/dashboard', window.__initialSource, window.__initialEnabled)
        `
      },
    }],
    build: {
      write: false,
      emptyOutDir: false,
      minify: false,
      lib: { entry: 'virtual:version-check-harness', formats: ['iife'], name: 'VersionCheckHarness' },
      rollupOptions: { input: 'virtual:version-check-harness' },
    },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  harnessScript = bundle.output.find((item) => item.type === 'chunk').code
  browser = await chromium.launch({ headless: true })
})

after(async () => { await browser?.close() })

async function fixture(t, frontend = 'v3.0.1', cached = null, legacy = null, source = 'patched', enabled = true) {
  const page = await browser.newPage()
  page.setDefaultTimeout(5_000)
  t.after(async () => { await page.close() })
  await page.route('http://version-check.test/**', route => route.fulfill({
    contentType: 'text/html', body: '<!doctype html><div id="root"></div>',
  }))
  await page.goto('http://version-check.test/')
  await page.evaluate(({ frontend, cached, legacy, source, enabled, CACHE_KEY, LEGACY_KEY }) => {
    window.__frontendVersion = frontend
    window.__initialSource = source
    window.__initialEnabled = enabled
    window.__requests = []
    window.__request = (kind, options) => new Promise((resolve, reject) => window.__requests.push({ kind, options, resolve, reject }))
    window.__intervals = []
    const originalSetInterval = window.setInterval.bind(window)
    window.setInterval = (callback, delay) => {
      window.__intervals.push({ callback, delay })
      return originalSetInterval(callback, delay)
    }
    if (cached) localStorage.setItem(CACHE_KEY, JSON.stringify({ checkedAt: Date.now(), ...cached }))
    if (legacy) localStorage.setItem(LEGACY_KEY, JSON.stringify(legacy))
  }, { frontend, cached, legacy, source, enabled, CACHE_KEY, LEGACY_KEY })
  await page.addScriptTag({ content: harnessScript })
  await page.waitForFunction(() => Boolean(window.__versionState))
  return page
}

async function waitRequests(page, count) {
  await page.waitForFunction(count => window.__requests.length === count, count)
  return page.evaluate(() => window.__requests.map(({ kind }) => kind))
}

async function settle(page, index, response, failed = false) {
  await page.evaluate(({ index, response, failed }) => {
    const request = window.__requests[index]
    if (failed) request.reject(new Error('controlled request failure'))
    else request.resolve(response)
  }, { index, response, failed })
}

async function waitState(page, expected) {
  await page.waitForFunction(expected => Object.entries(expected).every(([key, value]) => window.__versionState[key] === value), expected)
  return page.evaluate(() => window.__versionState)
}

function health(buildVersion) {
  return { status: 'ok', available: 1, total: 1, ...(buildVersion === undefined ? {} : { build_version: buildVersion }) }
}

function update(latest = 'patched-v1.0.1', overrides = {}) {
  return {
    source: 'patched', status: 'current', current_version: 'patched-v1.0.1', latest_version: latest,
    has_update: false, supported: true, target_tag: latest, plan_token: 'in-memory-only',
    plan_expires_at: new Date(Date.now() + 60_000).toISOString(),
    runtime_os: 'linux', runtime_arch: 'amd64', mode: 'binary',
    release_url: `https://example.test/releases/${latest}`, ...overrides,
  }
}

async function assertNoPersistedPlans(page) {
  assert.deepEqual(await page.evaluate(() => Object.keys(localStorage)), [])
}

test('persisted release caches cannot hide runtime identity or authorize a fork update', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '9.9.9', plan_token: 'stale' }, { current_version: '3.0.1' })
  assert.deepEqual(await waitRequests(page, 2), ['health', 'update'])
  assert.deepEqual(await page.evaluate(() => window.__requests.map(({ options }) => options)), [{ timeoutMs: 5_000 }, 'patched'])
  await settle(page, 0, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: null, hasUpdate: false, versionMismatch: true })
  await settle(page, 1, update())
  const state = await waitState(page, { latestVersion: 'patched-v1.0.1', hasUpdate: false })
  assert.equal(state.frontendVersion, 'v3.0.1')
  assert.equal(state.updateInfo.plan_token, 'in-memory-only')
  assert.equal(await page.evaluate(key => JSON.parse(localStorage.getItem(key)).plan_token, CACHE_KEY), 'stale')
})

test('in-memory plans do not cache runtime identity and equivalent prefixes preserve the frontend label', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('patched-v1.0.2', { status: 'available', has_update: true }))
  await waitState(page, { currentVersion: 'v3.0.4', hasUpdate: true, versionMismatch: true })
  await page.evaluate(() => { void window.__refresh() })
  assert.deepEqual(await waitRequests(page, 3), ['health', 'update', 'health'])
  await settle(page, 2, health('refs/tags/3.0.1'))
  await waitState(page, { currentVersion: 'v3.0.1', hasUpdate: true, versionMismatch: false })
  await assertNoPersistedPlans(page)
})

test('health resolves while release discovery is pending and preserves backend support diagnostics', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: null, hasUpdate: false, versionMismatch: true })
  await settle(page, 1, update('patched-v1.0.1', { supported: false, unsupported_reason: 'No release asset for ARMv6' }))
  const state = await waitState(page, { latestVersion: 'patched-v1.0.1', hasUpdate: false })
  assert.equal(state.updateInfo.supported, false)
  assert.equal(state.updateInfo.unsupported_reason, 'No release asset for ARMv6')
  assert.equal(state.updateInfo.current_version, 'patched-v1.0.1')
  await assertNoPersistedPlans(page)
})

test('failed, legacy and non-release health responses fall back without retaining an old runtime version', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.5'))
  await settle(page, 1, update())
  await waitState(page, { currentVersion: 'v3.0.5', hasUpdate: false, loading: false })
  for (const [index, version] of [undefined, 'dev', 'local-20261002-1234-abc', 'bad-version', 'failed'].entries()) {
    await page.evaluate(() => { void window.__refresh() })
    await waitRequests(page, index + 3)
    await settle(page, index + 2, health(version), version === 'failed')
    await waitState(page, { currentVersion: 'v3.0.1', hasUpdate: false, versionMismatch: false })
  }
})

test('dev and local builds preserve fork migration discovery without replacing their build labels', async t => {
  for (const frontend of ['dev', 'dev-abc123', 'local-20261002-1234-abc']) {
    const page = await fixture(t, frontend)
    await waitRequests(page, 2)
    await settle(page, 0, health('3.0.5'))
    await settle(page, 1, update('patched-v1.0.1', { status: 'migration', requires_migration_confirmation: true }))
    const state = await waitState(page, { currentVersion: frontend, latestVersion: 'patched-v1.0.1', hasUpdate: false, versionMismatch: false })
    assert.equal(state.updateInfo.requires_migration_confirmation, true)
  }
})

test('patched backend diagnostics keep exact tag identity and server-owned update eligibility', async t => {
  const page = await fixture(t, 'patched-v1.0.0')
  await waitRequests(page, 2)
  await settle(page, 0, health('patched-v1.0.1'))
  await settle(page, 1, update('patched-v99.0.0', { status: 'unavailable', has_update: true, supported: false }))
  await waitState(page, { currentVersion: 'patched-v1.0.1', latestVersion: 'patched-v99.0.0', versionMismatch: true, hasUpdate: false })
})

test('a newer route check owns runtime state and the source cache when older responses arrive last', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await page.evaluate(() => window.__render('/settings'))
  assert.deepEqual(await waitRequests(page, 4), ['health', 'update', 'health', 'update'])
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update())
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.1', hasUpdate: false })
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('patched-v9.0.0', { status: 'available', has_update: true }))
  await page.evaluate(() => { void window.__refresh() })
  await waitRequests(page, 5)
  await settle(page, 4, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.1', hasUpdate: false })
  await assertNoPersistedPlans(page)
})

test('responses from an unmounted instance cannot change a later mount or its cache', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await page.evaluate(() => { window.__unmount(); window.__mount('/settings') })
  await waitRequests(page, 4)
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update())
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.1', hasUpdate: false })
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('patched-v9.0.0'))
  await page.evaluate(() => { void window.__refresh() })
  await waitRequests(page, 5)
  await settle(page, 4, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.1', hasUpdate: false })
  await assertNoPersistedPlans(page)
})

test('failed forced discovery clears plans but never hides fresh independent runtime identity', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('patched-v1.0.2', { status: 'available', has_update: true }))
  await waitState(page, { hasUpdate: true })
  await page.evaluate(() => { void window.__refresh(true) })
  await waitRequests(page, 4)
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, null, true)
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: null, updateInfo: null, hasUpdate: false, error: true })
  await assertNoPersistedPlans(page)
})

test('periodic checks retain their interval while route refreshes bypass the source cache', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.5'))
  await settle(page, 1, update())
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.1', loading: false })
  assert.deepEqual(await page.evaluate(() => window.__intervals.map(({ delay }) => delay)), [30 * 60_000])
  await page.evaluate(() => window.__intervals[0].callback())
  assert.deepEqual(await waitRequests(page, 3), ['health', 'update', 'health'])
  await settle(page, 2, health('3.0.5'))
  await page.evaluate(() => window.__render('/settings'))
  assert.deepEqual(await waitRequests(page, 5), ['health', 'update', 'health', 'health', 'update'])
  await settle(page, 3, health('3.0.5'))
  await settle(page, 4, update('patched-v1.0.2', { status: 'available', has_update: true }))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'patched-v1.0.2', hasUpdate: true })
})

test('source switching isolates plans and wrong-source responses fail closed without fallback', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.5'))
  await settle(page, 1, update())
  await waitState(page, { latestVersion: 'patched-v1.0.1' })
  await page.evaluate(() => window.__render('/dashboard', 'official'))
  await waitRequests(page, 4)
  assert.equal(await page.evaluate(() => window.__requests[3].options), 'official')
  await waitState(page, { latestVersion: null, updateInfo: null })
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update('v3.0.6', { source: 'official', status: 'available', has_update: true }))
  const official = await waitState(page, { latestVersion: 'v3.0.6', hasUpdate: true })
  assert.equal(official.updateInfo.source, 'official')
  await page.evaluate(() => window.__render('/dashboard', 'patched'))
  await waitRequests(page, 5)
  await settle(page, 4, health('3.0.5'))
  await waitState(page, { latestVersion: 'patched-v1.0.1', hasUpdate: false })
  await page.evaluate(() => { void window.__refresh(true) })
  await waitRequests(page, 7)
  await settle(page, 5, health('3.0.5'))
  await settle(page, 6, update('v3.0.6', { source: 'official', status: 'available', has_update: true }))
  await waitState(page, { latestVersion: null, hasUpdate: false, error: true })
  assert.deepEqual(await page.evaluate(() => window.__requests.filter(r => r.kind === 'update').map(r => r.options)), ['patched', 'official', 'patched'])
  await assertNoPersistedPlans(page)
})

test('disabled modal hooks do not probe and cannot accept responses after closing', async t => {
  const page = await fixture(t, 'patched-v1.0.0', null, null, 'official', false)
  assert.deepEqual(await waitRequests(page, 0), [])
  assert.deepEqual(await page.evaluate(() => window.__intervals), [])
  await page.evaluate(() => window.__render('/dashboard', 'official', true))
  await waitRequests(page, 2)
  await page.evaluate(() => window.__render('/dashboard', 'official', false))
  // Drain the render/effect cleanup before resolving either pending request.
  await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)))
  await settle(page, 0, health('3.0.5'))
  await settle(page, 1, update('v3.0.6', { source: 'official', status: 'available', has_update: true }))
  await waitState(page, { currentVersion: 'patched-v1.0.0', latestVersion: null, hasUpdate: false })
})
