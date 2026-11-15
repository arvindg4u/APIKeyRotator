import { Hono, type Context } from 'hono'
import type { Env, ProxyConfig } from '../lib/types'
import { getConfigBySlug } from '../lib/db'
import { proxyKeys } from '../lib/env'
import { forwardWithRotation } from '../lib/forward'
import { constantTimeEquals } from '../lib/jwt'

export const llmRoutes = new Hono<{ Bindings: Env }>()

// Accept the proxy key from the same places the Go backend does, so native SDKs
// work unchanged: Authorization: Bearer, x-api-key / x-goog-api-key /
// x-anthropic-api-key, or ?key= (Gemini).
function extractProxyKey(c: Context<{ Bindings: Env }>): string {
  const auth = c.req.header('Authorization') || ''
  if (auth) return auth.startsWith('Bearer ') ? auth.slice(7) : auth
  for (const header of ['x-api-key', 'x-goog-api-key', 'x-anthropic-api-key']) {
    const value = c.req.header(header)
    if (value) return value
  }
  return c.req.query('key') || ''
}

function validProxyKey(c: Context<{ Bindings: Env }>, candidate: string): boolean {
  if (!candidate) return false
  return proxyKeys(c.env).some((key) => constantTimeEquals(key, candidate))
}

// Port of the OpenAI adapter URL building: base + action with duplicate /v1 removed.
function buildTarget(config: ProxyConfig, action: string, incoming: URL): URL {
  const base = (config.target_base_url || '').trim().replace(/\/+$/, '')
  if (!base) throw new Error('target base URL is not configured')

  let actionPath = action.replace(/^\/+/, '')
  while (base.endsWith('/v1') && actionPath.startsWith('v1/')) {
    actionPath = actionPath.slice(3).replace(/^\/+/, '')
  }
  if (actionPath === 'v1' && base.endsWith('/v1')) actionPath = ''

  const target = new URL(actionPath ? `${base}/${actionPath}` : base)
  for (const [k, v] of incoming.searchParams) target.searchParams.set(k, v)
  // Never forward the proxy key upstream; the upstream key is injected later.
  target.searchParams.delete('key')
  return target
}

function forwardedHeaders(c: Context<{ Bindings: Env }>): Headers {
  const headers = new Headers()
  c.req.raw.headers.forEach((value, name) => {
    const lower = name.toLowerCase()
    if (
      lower === 'authorization' ||
      lower === 'host' ||
      lower === 'content-length' ||
      lower === 'accept-encoding' ||
      lower === 'connection' ||
      lower.startsWith('cf-') ||
      lower === 'x-real-ip' ||
      lower === 'x-forwarded-proto'
    ) {
      return
    }
    headers.set(name, value)
  })
  return headers
}

const handler = async (c: Context<{ Bindings: Env }>) => {
  const slug = c.req.param('slug') ?? ''

  if (!validProxyKey(c, extractProxyKey(c))) {
    return c.json({ detail: "Bad Request: invalid or missing proxy key (Authorization: Bearer, x-api-key, or ?key=)" }, 400)
  }

  const config = await getConfigBySlug(c.env.DB, slug)
  if (!config || !config.is_active) {
    return c.json({ detail: `LLM service configuration with slug '${slug}' not found or inactive` }, 400)
  }

  const url = new URL(c.req.url)
  const prefix = `/llm/${slug}`
  const action = url.pathname.startsWith(prefix) ? url.pathname.slice(prefix.length) : ''

  let target: URL
  try {
    target = buildTarget(config, action, url)
  } catch (error) {
    return c.json({ detail: error instanceof Error ? error.message : 'invalid target' }, 502)
  }

  const method = c.req.method.toUpperCase()
  const hasBody = method !== 'GET' && method !== 'HEAD'
  const body = hasBody ? await c.req.raw.arrayBuffer() : null

  return forwardWithRotation({
    env: c.env,
    config,
    method,
    target,
    headers: forwardedHeaders(c),
    body,
    executionCtx: c.executionCtx,
  })
}

llmRoutes.all('/:slug', handler)
llmRoutes.all('/:slug/*', handler)