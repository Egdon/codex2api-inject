import test from 'node:test'
import assert from 'node:assert/strict'
import { turnStateHistoryQuery } from './turnStateHistory.ts'

test('renewal history encodes proxy filters without losing scheme, port or special characters', () => {
  const address = 'socks5://[2001:db8::1]:1080'
  const query = new URLSearchParams(turnStateHistoryQuery(2, { account_id: 37, model: 'model+variant', status: 'failed', proxy_url: address, plan: '' }))
  assert.equal(query.get('proxy_url'), address)
  assert.equal(query.get('model'), 'model+variant')
  assert.equal(query.get('account_id'), '37')
  assert.equal(query.get('status'), 'failed')
  assert.equal(query.get('page'), '2')
  assert.equal(query.get('page_size'), '20')
  assert.equal(query.has('plan'), false)
})
