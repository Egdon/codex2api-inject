import assert from 'node:assert/strict'
import test from 'node:test'
import { hasRemoteUpdate, normalizeVersion, resolveBuildVersions, versionLabel } from './versionCheck.ts'
import { readFileSync } from 'node:fs'

test('patched release diagnostics preserve exact tags without authorizing cross-source updates', () => {
  assert.deepEqual(resolveBuildVersions('patched-v1.0.0', 'patched-v1.0.1'), {
    frontendVersion: 'patched-v1.0.0', currentVersion: 'patched-v1.0.1', versionMismatch: true,
  })
  assert.equal(resolveBuildVersions('patched-v1.0.0', 'patched-v1.0.0').versionMismatch, false)
  assert.equal(resolveBuildVersions('patched-v1.0.0', '3.0.6').versionMismatch, true)
  assert.equal(resolveBuildVersions('patched-v1.0.0', 'patched-v01.0.1').versionMismatch, false)
  for (const version of ['patched-v1.0.0', 'dev-abc123', 'local-20261002-1234-abc']) {
    assert.equal(versionLabel(version), version)
  }
  assert.equal(versionLabel('3.0.6'), 'v3.0.6')
  const hook = readFileSync(new URL('../hooks/useVersionCheck.ts', import.meta.url), 'utf8')
  assert.match(hook, /source: UpdateSource = 'patched'/)
  assert.match(hook, /api\.getSystemUpdate\(source\)/)
  assert.match(hook, /info\.source !== source/)
  assert.match(hook, /new Map<UpdateSource/)
  assert.match(hook, /hasUpdate: updateInfo\?\.status === 'available' && updateInfo\.has_update/)
  assert.doesNotMatch(hook, /localStorage|hasRemoteUpdate/)
})

test('runtime diagnostics do not bypass the fork updater confirmation and provenance checks', () => {
  const layout = readFileSync(new URL('../components/Layout.tsx', import.meta.url), 'utf8')
  const modal = readFileSync(new URL('../components/SystemUpdateModal.tsx', import.meta.url), 'utf8')
  assert.match(layout, /<SystemUpdateModal/)
  assert.doesNotMatch(layout, /performSystemUpdate|releases\/tag/)
  assert.match(modal, /useState<UpdateSource>\('patched'\)/)
  assert.match(modal, /source: plan\.source, target_tag: plan\.target_tag, plan_token: plan\.plan_token/)
  assert.match(modal, /confirm_official: officialConfirmed, confirm_migration: migrationConfirmed/)
  assert.match(modal, /current\.source === plan\.source && exactVersion\(current\.version\) === target/)
  assert.match(modal, /current\.revision !== before\.revision/)
  assert.match(modal, /build\?\.upstream_base/)
  assert.match(modal, /api\.getSystemBuild\(\)/)
})

test('an old frontend uses the running release for cached latest-version comparison', () => {
  const current = resolveBuildVersions('v3.0.1', '3.0.5')
  assert.deepEqual(current, {
    frontendVersion: 'v3.0.1', currentVersion: 'v3.0.5', versionMismatch: true,
  })
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.5'), false)
  assert.equal(hasRemoteUpdate(resolveBuildVersions('v3.0.1', '3.0.4').currentVersion, '3.0.5'), true)
})

test('matching release prefixes preserve the original frontend label', () => {
  for (const frontend of ['v3.0.5', '3.0.5', 'V3.0.5', 'refs/tags/v3.0.5']) {
    for (const backend of ['v3.0.5', '3.0.5', 'V3.0.5', ' refs/tags/v3.0.5 ']) {
      assert.equal(normalizeVersion(frontend), normalizeVersion(backend))
      assert.deepEqual(resolveBuildVersions(frontend, backend), {
        frontendVersion: frontend, currentVersion: frontend, versionMismatch: false,
      })
    }
  }
})

test('missing, development and malformed backend identities keep the frontend behavior', () => {
  for (const backend of [undefined, null, '', 'dev', 'local-20261002-1234-abc', 'main', '3.0', '3.0.5oops', '01.0.5', '3.0.5-01', '3.0.5+']) {
    const versions = resolveBuildVersions('v3.0.1', backend)
    assert.equal(versions.currentVersion, 'v3.0.1', String(backend))
    assert.equal(versions.versionMismatch, false, String(backend))
    assert.equal(hasRemoteUpdate(versions.currentVersion, '3.0.5'), true)
  }
})

test('development and local frontend builds retain their existing update policy', () => {
  const dev = resolveBuildVersions('dev', 'v3.0.5')
  assert.equal(dev.currentVersion, 'dev')
  assert.equal(dev.versionMismatch, false)
  assert.equal(hasRemoteUpdate(dev.currentVersion, '3.0.5'), false)

  const local = resolveBuildVersions('local-20261002-1234-abc', 'v3.0.5')
  assert.equal(local.currentVersion, 'local-20261002-1234-abc')
  assert.equal(local.versionMismatch, false)
  assert.equal(hasRemoteUpdate(local.currentVersion, '3.0.5'), true)
  assert.equal(resolveBuildVersions('branch-preview', 'v3.0.5').currentVersion, 'branch-preview')
})

test('release build metadata remains diagnostic while numeric update comparison stays compatible', () => {
  const current = resolveBuildVersions('v3.0.1', 'refs/tags/v3.0.5-rc.1+build.7')
  assert.equal(current.currentVersion, 'v3.0.5-rc.1+build.7')
  assert.equal(current.versionMismatch, true)
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.5'), false)
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.6'), true)
  assert.equal(hasRemoteUpdate('3.0', '3.0.1'), true)
  assert.equal(hasRemoteUpdate('v3.0.6', '3.0.5'), false)
  assert.equal(hasRemoteUpdate('v3.0.1', null), false)
})
