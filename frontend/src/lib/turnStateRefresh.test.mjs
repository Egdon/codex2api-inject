import assert from 'node:assert/strict'
import test from 'node:test'
import { readTurnStateRefresh } from './turnStateRefresh.ts'

function stream(wire) {
  const bytes = new TextEncoder().encode(wire)
  return new ReadableStream({ start(controller) {
    for (let i = 0; i < bytes.length; i += 2) controller.enqueue(bytes.slice(i, i + 2))
    controller.close()
  } })
}

test('template refresh preserves per-model failures across split UTF-8 and CRLF', async () => {
  const events = []
  await readTurnStateRefresh(stream('data: {"type":"result","result":{"model":"gpt-5.6-luna","saved":false,"error":"获取失败"}}\r\n\r\ndata: {"type":"done","saved":0,"total":1}\n\n'), event => events.push(event))
  assert.equal(events[0].result.error, '获取失败')
  assert.equal(events[1].saved, 0)
})

test('an interrupted acquisition cannot report success', async () => {
  await assert.rejects(readTurnStateRefresh(stream('data: {"type":"testing","model":"gpt-6-astra"}\n\n'), () => {}), /incomplete_stream/)
})
