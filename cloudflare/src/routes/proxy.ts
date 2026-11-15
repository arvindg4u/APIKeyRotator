import { Hono, type Context } from 'hono'
import type { Env } from '../lib/types'
import { getConfigBySlug } from '../lib/db'
import { proxyKeys } from '../lib/env'
import { forwardWithRotation } from '../lib/forward'
import { constantTimeEquals } from '../lib/jwt'

export const proxyRoutes = new Hono<{ Bindings: Env }>()

// Generic REST proxy: /proxy/<slug>/<rest...> -> config.target_url + /<rest...>
function extractProxyKey(c: Context<{ Bindings: Env }>): string {
  const auth = c.req.header('Authorization') || ''
  if (auth) return auth.startsWith('Bearer ') ? auth.slice(7) : auth
  for (const header of ['x-api-key', 'x-goog-api-key', 'x-anthropic-api-key']) {
    const value = c.req.header(header)
    if (value) return value
  }
  return c.req.query('key') || ''
}

const handler = async (c: Context<{ Bindings: Env }>) => {
  const incoming = new URL(c.req.url)
  // Strip the /proxy/ prefix, then the first segment is the slug.
  const rest = incoming.pathname.replace(/^\/proxy\/?/, '')
  const slash = rest.indexOf('/')
  const slug = slash >= 0 ? rest.slice(0, slash) : rest
  const tail = slash >= 0 ? rest.slice(slash) : ''

  if (!slug) return c.json({ error: 'missing slug' }, 400)

  const candidate = extractProxyKey(c)
  if (!candidate || !proxyKeys(c.env).some((key) => constantTimeEquals(key, candidate))) {
    return c.json({ detail: 'invalid or missing proxy key' }, 400)
  }

  const config = await getConfigBySlug(c.env.DB, slug)
  if (!config || !config.is_active) {
    return c.json({ detail: `proxy configuration with slug '${slug}' not found or inactive` }, 400)
  }

  const base = (config.target_url || '').trim().replace(/\/+$/, '')
  if (!base) return c.json({ detail: 'target URL is not configured' }, 502)

  const target = new URL(`${base}${tail}`)
  for (const [k, v] of incoming.searchParams) target.searchParams.set(k, v)
  target.searchParams.delete('key')

  const method = (config.method || c.req.method).toUpperCase()
  const hasBody = method !== 'GET' && method !== 'HEAD'
  const body = hasBody ? await c.req.raw.arrayBuffer() : null

  const headers = new Headers()
  c.req.raw.headers.forEach((value, name) => {
    const lower = name.toLowerCase()
    if (
      lower === 'authorization' ||
      lower === 'host' ||
      lower === 'content-length' ||
      lower === 'accept-encoding' ||
      lower === 'connection' ||
      lower.startsWith('cf-')
    ) {
      return
    }
    headers.set(name, value)
  })

  return forwardWithRotation({
    env: c.env,
    config,
    method,
    target,
    headers,
    body,
    executionCtx: c.executionCtx,
  })
}

proxyRoutes.all('/*', handler)