import type { CodexTurnStateStatus } from '../types'

export interface TurnStateRefreshEvent {
  type: 'start' | 'testing' | 'result' | 'done'
  model?: string
  result?: { model: string; saved: boolean; error?: string }
  saved?: number
  total?: number
  status?: CodexTurnStateStatus
}

export async function readTurnStateRefresh(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: TurnStateRefreshEvent) => void,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let completed = false
  const consume = (line: string) => {
    if (!line.startsWith('data:')) return
    const event = JSON.parse(line.slice(5).trim()) as TurnStateRefreshEvent
    if (event.type === 'done') completed = true
    onEvent(event)
  }
  try {
    for (;;) {
      const { done, value } = await reader.read()
      buffer += done ? decoder.decode() : decoder.decode(value, { stream: true })
      let end: number
      while ((end = buffer.indexOf('\n')) >= 0) {
        consume(buffer.slice(0, end).replace(/\r$/, ''))
        buffer = buffer.slice(end + 1)
      }
      if (done) break
    }
    if (buffer.trim()) consume(buffer)
    if (!completed) throw new Error('incomplete_stream')
  } finally {
    reader.releaseLock()
  }
}
