// Server-Sent Events 客户端。
//
// 不用原生 EventSource: 管理接口用 `Authorization: Bearer <jwt>` 鉴权,
// 而 EventSource 构造函数无法设置请求头, 只能把令牌塞进 URL, 那样会出现在
// nginx 访问日志/浏览器历史里。这里用 fetch + ReadableStream, 可以正常带
// 请求头, 并自己实现断线重连 (指数退避 + 抖动)。
const STREAM_URL = '/admin/status/stream'

export function openStatusStream({ onOpen, onEvent, onLog, onError } = {}) {
  const controller = new AbortController()
  let stopped = false
  let attempt = 0

  const run = async () => {
    while (!stopped) {
      try {
        const token = localStorage.getItem('authToken') || ''
        const res = await fetch(STREAM_URL, {
          method: 'GET',
          headers: {
            Authorization: `Bearer ${token}`,
            Accept: 'text/event-stream',
          },
          cache: 'no-store',
          signal: controller.signal,
        })

        if (res.status === 401) {
          localStorage.removeItem('authToken')
          if (window.location.pathname !== '/login') {
            window.location.href = '/login'
          }
          return
        }
        if (!res.ok || !res.body) {
          throw new Error(`status stream HTTP ${res.status}`)
        }

        attempt = 0
        onOpen && onOpen()
        await readStream(res.body, { onEvent, onLog, isStopped: () => stopped })
      } catch (err) {
        if (stopped || controller.signal.aborted) return
        onError && onError(err)
      }

      if (stopped) return
      attempt += 1
      const base = Math.min(30000, 1000 * Math.pow(2, Math.min(attempt, 5)))
      const delay = base + Math.random() * 500
      await sleep(delay)
    }
  }

  run()

  return () => {
    stopped = true
    controller.abort()
  }
}

async function readStream(body, { onEvent, onLog, isStopped }) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  for (;;) {
    if (isStopped()) {
      try { await reader.cancel() } catch (e) { /* ignore */ }
      return
    }
    const { value, done } = await reader.read()
    if (done) return

    buffer += decoder.decode(value, { stream: true })
    // SSE 记录之间用空行分隔; 统一换行符后按 '\n\n' 切分。
    buffer = buffer.replace(/\r\n/g, '\n')
    const chunks = buffer.split('\n\n')
    buffer = chunks.pop() || ''

    for (const chunk of chunks) {
      const frame = parseFrame(chunk)
      if (!frame) continue
      if (frame.event === 'event') {
        const data = safeParse(frame.data)
        if (data) onEvent && onEvent(data)
      } else if (frame.event === 'log') {
        const data = safeParse(frame.data)
        if (data) onLog && onLog(data)
      }
    }
  }
}

function parseFrame(raw) {
  let event = 'message'
  const data = []
  for (const line of raw.split('\n')) {
    if (!line || line.startsWith(':')) continue
    if (line.startsWith('event:')) {
      event = line.slice(6).trim()
    } else if (line.startsWith('data:')) {
      data.push(line.slice(5).replace(/^ /, ''))
    }
  }
  if (data.length === 0) return null
  return { event, data: data.join('\n') }
}

function safeParse(text) {
  try {
    return JSON.parse(text)
  } catch (e) {
    return null
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}