import { Hono, type Context, type MiddlewareHandler } from 'hono'
import type { Env, ProxyConfig, ProxyConfigInput, StoredEvent, StoredLog } from '../lib/types'
import * as db from '../lib/db'
import { signToken, verifyToken, constantTimeEquals } from '../lib/jwt'
import { defaultUserAgent, proxyKeys, rotator } from '../lib/env'
import { apiFormatOf, injectKey } from '../lib/keys'

const BOOT = Date.now()

export const adminRoutes = new Hono<{ Bindings: Env }>()

const requireAdmin: MiddlewareHandler<{ Bindings: Env }> = async (c, next) => {
  const header = c.req.header('Authorization') || ''
  const token = header.startsWith('Bearer ') ? header.slice(7).trim() : (c.req.header('X-Admin-Token') || '').trim()
  const secret = c.env.JWT_SECRET || ''
  if (!token || !secret) {
    return c.json({ error: 'Unauthorized', success: false }, 401)
  }
  const username = await verifyToken(secret, token)
  if (!username) {
    return c.json({ error: 'Invalid or expired token', success: false }, 401)
  }
  await next()
}

// --- public ---

adminRoutes.post('/login', async (c) => {
  const body = (await c.req.json().catch(() => null)) as { username?: string; password?: string } | null
  if (!body || typeof body.username !== 'string' || typeof body.password !== 'string') {
    return c.json({ error: 'Invalid request' }, 400)
  }
  const adminUsername = c.env.ADMIN_USERNAME || 'admin'
  const adminPassword = c.env.ADMIN_PASSWORD
  const secret = c.env.JWT_SECRET
  if (!adminPassword || !secret) {
    return c.json({ error: 'Server not configured (ADMIN_PASSWORD / JWT_SECRET)', success: false }, 500)
  }
  if (constantTimeEquals(body.username, adminUsername) && constantTimeEquals(body.password, adminPassword)) {
    const token = await signToken(secret, adminUsername, 86400)
    return c.json({ message: 'Login successful', success: true, token, expires_in: 86400 })
  }
  return c.json({ error: 'Invalid credentials', success: false }, 401)
})

adminRoutes.get('/app-config', (c) => {
  const base = c.env.PROXY_PUBLIC_BASE_URL || new URL(c.req.url).origin
  return c.json({ proxy_public_base_url: base })
})

// --- protected ---

const protectedApp = new Hono<{ Bindings: Env }>()
protectedApp.use('*', requireAdmin)

protectedApp.get('/proxy-configs', async (c) => {
  return c.json(await db.listConfigs(c.env.DB))
})

protectedApp.post('/proxy-configs', async (c) => {
  const body = (await c.req.json().catch(() => null)) as ProxyConfigInput | null
  if (!body?.name || !body?.slug || !body?.config_type) {
    return c.json({ error: 'name, slug and config_type are required' }, 400)
  }
  const config = await db.createConfig(c.env.DB, body)
  return c.json(config, 201)
})

protectedApp.get('/proxy-configs/:id', async (c) => {
  const config = await db.getConfigById(c.env.DB, Number(c.req.param('id')))
  if (!config) return c.json({ error: 'Config not found' }, 404)
  return c.json(config)
})

protectedApp.put('/proxy-configs/:id', async (c) => {
  const id = Number(c.req.param('id'))
  const body = (await c.req.json().catch(() => null)) as ProxyConfigInput | null
  if (!body?.name || !body?.slug || !body?.config_type) {
    return c.json({ error: 'name, slug and config_type are required' }, 400)
  }
  const config = await db.updateConfig(c.env.DB, id, body)
  if (!config) return c.json({ error: 'Config not found' }, 404)
  return c.json(config)
})

protectedApp.put('/proxy-configs/:id/status', async (c) => {
  const body = (await c.req.json().catch(() => null)) as { is_active?: boolean } | null
  await db.updateConfigStatus(c.env.DB, Number(c.req.param('id')), Boolean(body?.is_active))
  return c.json({ success: true })
})

protectedApp.delete('/proxy-configs/:id', async (c) => {
  await db.deleteConfig(c.env.DB, Number(c.req.param('id')))
  return c.json({ success: true })
})

protectedApp.get('/proxy-configs/:id/keys', async (c) => {
  return c.json(await db.listKeys(c.env.DB, Number(c.req.param('id'))))
})

protectedApp.post('/proxy-configs/:id/keys', async (c) => {
  const id = Number(c.req.param('id'))
  const body = (await c.req.json().catch(() => null)) as { key_value?: string; is_active?: boolean } | null
  if (!body?.key_value) return c.json({ error: 'key_value is required' }, 400)
  return c.json(await db.createKey(c.env.DB, id, body.key_value, Boolean(body.is_active)), 201)
})

protectedApp.post('/proxy-configs/:id/keys/batch', async (c) => {
  const id = Number(c.req.param('id'))
  const body = (await c.req.json().catch(() => null)) as { keys?: string[]; is_active?: boolean } | null
  if (!Array.isArray(body?.keys) || body.keys.length === 0) {
    return c.json({ error: 'keys array is required' }, 400)
  }
  return c.json(await db.batchCreateKeys(c.env.DB, id, body.keys, body.is_active !== false))
})

protectedApp.delete('/proxy-configs/:id/keys', async (c) => {
  const deleted = await db.clearKeys(c.env.DB, Number(c.req.param('id')))
  return c.json({ deleted_count: deleted })
})

protectedApp.patch('/keys/:keyID', async (c) => {
  const body = (await c.req.json().catch(() => null)) as { is_active?: boolean } | null
  await db.updateKeyStatus(c.env.DB, Number(c.req.param('keyID')), Boolean(body?.is_active))
  return c.json({ success: true })
})

protectedApp.delete('/keys/:keyID', async (c) => {
  await db.deleteKey(c.env.DB, Number(c.req.param('keyID')))
  return c.json({ success: true })
})

// Upstream model list, using the config's key pool (rotates past rejected keys).
protectedApp.get('/proxy-configs/:id/models', async (c) => {
  const id = Number(c.req.param('id'))
  const config = await db.getConfigById(c.env.DB, id)
  if (!config) return c.json({ error: 'Config not found' }, 404)
  if (!config.config_type.toLowerCase().includes('llm')) {
    return c.json({ error: 'Model listing is only available for LLM services' }, 400)
  }
  const base = (config.target_base_url || '').trim().replace(/\/+$/, '')
  if (!base) return c.json({ error: 'target base URL is not configured' }, 400)

  const keys = (config.api_keys ?? []).filter((k) => k.is_active)
  if (!keys.length) return c.json({ error: 'no active API keys for this service' }, 400)

  const rpc = rotator(c.env, config.slug)
  const format = apiFormatOf(config)
  const tried: number[] = []
  for (let attempt = 0; attempt < Math.min(6, keys.length); attempt++) {
    const pick = await rpc.next(keys.map((k) => k.id), tried)
    if (!pick) break
    tried.push(pick.id)
    const key = keys.find((k) => k.id === pick.id)!
    const headers = new Headers({ Accept: 'application/json', 'User-Agent': defaultUserAgent(c.env) })
    const params = new URLSearchParams()
    if (format === 'anthropic_native') headers.set('anthropic-version', '2023-06-01')
    if (!injectKey(headers, params, config, format, key.key_value)) break
    const url = params.toString() ? `${base}/models?${params}` : `${base}/models`
    try {
      const res = await fetch(url, { headers })
      if (res.ok) {
        const payload = (await res.json().catch(() => null)) as { data?: Array<{ id?: string; name?: string }> } | null
        const list = (payload?.data ?? []).map((m) => ({
          id: String(m.id ?? ''),
          name: String(m.name ?? m.id ?? ''),
          context_window: null as number | null,
        }))
        return c.json(list)
      }
    } catch {
      /* try next key */
    }
  }
  return c.json({ error: 'Failed to fetch model list' }, 502)
})

// --- status ---

protectedApp.get('/status/overview', async (c) => {
  const configs = await db.listConfigs(c.env.DB)
  let keys = 0
  let activeKeys = 0
  let coolingKeys = 0
  let totalRequests = 0
  let activeConfigs = 0
  const perConfig = []

  for (const cfg of configs) {
    let cfgKeys = 0
    let cfgActive = 0
    for (const key of cfg.api_keys ?? []) {
      cfgKeys += 1
      keys += 1
      if (key.is_active) {
        cfgActive += 1
        activeKeys += 1
      }
    }
    let cfgCooling = 0
    if (cfgActive > 0) {
      const snap = await rotator(c.env, cfg.slug).snapshot()
      cfgCooling = snap.cooling
      coolingKeys += snap.cooling
      totalRequests += snap.total
    }
    if (cfg.is_active) activeConfigs += 1
    perConfig.push({
      id: cfg.id,
      name: cfg.name,
      slug: cfg.slug,
      type: cfg.config_type,
      active: cfg.is_active,
      keys: cfgKeys,
      active_keys: cfgActive,
      cooling_keys: cfgCooling,
    })
  }

  return c.json({
    status: 'ok',
    time: new Date().toISOString(),
    started_at: new Date(BOOT).toISOString(),
    uptime_seconds: Math.floor((Date.now() - BOOT) / 1000),
    version: 'worker',
    go_version: 'workers',
    cache_type: 'durable-object',
    db_type: 'd1',
    egress_enabled: false,
    configs: configs.length,
    active_configs: activeConfigs,
    keys,
    active_keys: activeKeys,
    cooling_keys: coolingKeys,
    total_requests: totalRequests,
    per_config: perConfig,
  })
})

protectedApp.get('/status/traffic', async (c) => {
  const configs = await db.listConfigs(c.env.DB)
  const statusCounts: Record<string, number> = {}
  const rpm = new Array(60).fill(0)
  let total = 0
  const perSlug = []

  for (const cfg of configs) {
    if (!(cfg.api_keys ?? []).some((k) => k.is_active)) continue
    const snap = await rotator(c.env, cfg.slug).snapshot()
    total += snap.total
    let clientErrors = 0
    let serverErrors = 0
    let rateLimited = 0
    for (const [code, count] of Object.entries(snap.statuses)) {
      statusCounts[code] = (statusCounts[code] ?? 0) + count
      const n = Number(code)
      if (n === 429) rateLimited += count
      else if (n >= 500) serverErrors += count
      else if (n >= 400) clientErrors += count
    }
    for (let i = 0; i < rpm.length; i++) rpm[i] += snap.rpm[i] ?? 0
    perSlug.push({
      slug: cfg.slug,
      requests: snap.total,
      rate_limited: rateLimited,
      client_errors: clientErrors,
      server_errors: serverErrors,
      avg_latency_ms: 0,
    })
  }

  const classes: Record<string, number> = { '1xx': 0, '2xx': 0, '3xx': 0, '4xx': 0, '5xx': 0, other: 0 }
  for (const [code, count] of Object.entries(statusCounts)) {
    const n = Number(code)
    const bucket = n >= 100 && n < 600 ? `${Math.floor(n / 100)}xx` : 'other'
    classes[bucket] = (classes[bucket] ?? 0) + count
  }

  perSlug.sort((a, b) => (b.requests - a.requests) || a.slug.localeCompare(b.slug))

  return c.json({
    since: new Date(BOOT).toISOString(),
    total_requests: total,
    avg_latency_ms: 0,
    status_classes: classes,
    status_counts: statusCounts,
    requests_per_minute: rpm,
    per_slug: perSlug,
  })
})

function eventWithId(cfgId: number, event: StoredEvent): StoredEvent {
  return { ...event, id: cfgId * 1_000_000 + event.id }
}

function logWithId(cfgId: number, log: StoredLog): StoredLog {
  return { ...log, id: cfgId * 1_000_000 + log.id }
}

async function collectEvents(env: Env, limit: number): Promise<StoredEvent[]> {
  const configs = await db.listConfigs(env.DB)
  const all: StoredEvent[] = []
  for (const cfg of configs) {
    const events = await rotator(env, cfg.slug).eventsSince(0, limit)
    for (const e of events) all.push(eventWithId(cfg.id, e))
  }
  all.sort((a, b) => a.time - b.time)
  return all.slice(-limit)
}

async function collectLogs(env: Env, limit: number): Promise<StoredLog[]> {
  const configs = await db.listConfigs(env.DB)
  const all: StoredLog[] = []
  for (const cfg of configs) {
    const logs = await rotator(env, cfg.slug).logsSince(0, limit)
    for (const l of logs) all.push(logWithId(cfg.id, l))
  }
  all.sort((a, b) => a.time - b.time)
  return all.slice(-limit)
}

function requestLimit(c: Context<{ Bindings: Env }>): number {
  const raw = Number(c.req.query('limit') ?? 200)
  if (!Number.isFinite(raw) || raw <= 0) return 200
  return Math.min(500, raw)
}

protectedApp.get('/status/events', async (c) => {
  return c.json({ events: await collectEvents(c.env, requestLimit(c)) })
})

protectedApp.get('/status/logs', async (c) => {
  return c.json({ logs: await collectLogs(c.env, requestLimit(c)) })
})

// SSE: poll each config's Durable Object and forward new events/logs.
protectedApp.get('/status/stream', async (c) => {
  const env = c.env
  const configs = await db.listConfigs(env.DB)
  const encoder = new TextEncoder()

  const body = new ReadableStream({
    start(controller) {
      let closed = false
      let lastEventTime = Date.now()
      let lastLogTime = Date.now()

      const send = (name: string, data: unknown) => {
        if (closed) return
        try {
          controller.enqueue(encoder.encode(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`))
        } catch {
          closed = true
        }
      }

      const tick = async () => {
        if (closed) return
        for (const cfg of configs) {
          const rpc = rotator(env, cfg.slug)
          try {
            const events = await rpc.eventsSince(0, 50)
            for (const e of events) {
              if (e.time > lastEventTime) send('event', eventWithId(cfg.id, e))
            }
            const logs = await rpc.logsSince(0, 50)
            for (const l of logs) {
              if (l.time > lastLogTime) send('log', logWithId(cfg.id, l))
            }
          } catch {
            /* ignore a transient DO error */
          }
        }
        lastEventTime = Date.now()
        lastLogTime = Date.now()
      }

      const heartbeat = setInterval(() => {
        if (closed) return
        try {
          controller.enqueue(encoder.encode(': heartbeat\n\n'))
        } catch {
          closed = true
        }
        void tick()
      }, 3000)

      const stop = () => {
        if (closed) return
        closed = true
        clearInterval(heartbeat)
        try {
          controller.close()
        } catch {
          /* already closed */
        }
      }

      c.req.raw.signal.addEventListener('abort', stop)
      send('ready', { ts: Date.now() })
    },
  })

  return new Response(body, {
    headers: {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache, no-transform',
      Connection: 'keep-alive',
      'X-Accel-Buffering': 'no',
    },
  })
})

adminRoutes.route('/', protectedApp)