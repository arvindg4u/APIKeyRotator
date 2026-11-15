import { Hono } from 'hono'
import type { Env } from './lib/types'
import { healthRoutes } from './routes/health'
import { adminRoutes } from './routes/admin'
import { llmRoutes } from './routes/llm'
import { proxyRoutes } from './routes/proxy'

// The Durable Object class must be exported from the Worker entrypoint.
export { Rotator } from './do/rotator'

const app = new Hono<{ Bindings: Env }>()

app.route('/health', healthRoutes)
app.route('/admin', adminRoutes)
app.route('/llm', llmRoutes)
app.route('/proxy', proxyRoutes)

app.notFound((c) => c.json({ error: 'Route not found' }, 404))
app.onError((err, c) => {
  console.error('worker error', err)
  return c.json({ error: 'Internal error', detail: String(err) }, 500)
})

export default app
