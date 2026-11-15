import type { Env, ProxyConfig } from './types'
import { cooldownSettings, defaultUserAgent, rotator } from './env'
import { apiFormatOf, injectKey } from './keys'
import { classifyRateLimit, type RateClassification } from './ratelimit'
import { maskKey } from './mask'

const MAX_ATTEMPTS = 45 // Workers Free: 50 subrequests per invocation
const MAX_ERROR_BODY = 1 << 20

// Retryable statuses are key/account scoped: rotating to another key can help.
// (429 rate limit / quota, 401 invalid key, 403 disabled key.)
function isRetryable(status: number): boolean {
  return status === 429 || status === 401 || status === 403
}

function clampWait(classification: RateClassification, settings: ReturnType<typeof cooldownSettings>): number {
  const fallback = classification.class === 'free_tier' ? settings.freeTier * 1000 : settings.transient * 1000
  const base = classification.waitMs > 0 ? classification.waitMs : fallback
  const max = classification.class === 'free_tier' ? settings.freeTierMax * 1000 : settings.transientMax * 1000
  return Math.min(base, max)
}

export interface ExecutionContextLike {
  waitUntil(promise: Promise<unknown>): void
}

export interface ForwardOptions {
  env: Env
  config: ProxyConfig
  method: string
  target: URL
  headers: Headers
  body: ArrayBuffer | null
  executionCtx: ExecutionContextLike
}

export async function forwardWithRotation(opts: ForwardOptions): Promise<Response> {
  const { env, config, method, target, headers, body, executionCtx } = opts
  const activeKeys = (config.api_keys ?? []).filter((k) => k.is_active)
  if (activeKeys.length === 0) {
    return Response.json({ detail: 'No active API keys for this service' }, { status: 502 })
  }

  const format = apiFormatOf(config)
  const rpc = rotator(env, config.slug)
  const ids = activeKeys.map((k) => k.id)
  const tried: number[] = []
  const settings = cooldownSettings(env)
  const started = Date.now()

  let last: { status: number; headers: Headers; body: ArrayBuffer } | null = null

  for (let attempt = 0; attempt < Math.min(MAX_ATTEMPTS, ids.length); attempt++) {
    const pick = await rpc.next(ids, tried)
    if (!pick) break
    tried.push(pick.id)

    const key = activeKeys.find((k) => k.id === pick.id)
    if (!key) continue

    const attemptHeaders = new Headers(headers)
    const params = new URLSearchParams(target.search)
    if (!injectKey(attemptHeaders, params, config, format, key.key_value)) break
    if (!attemptHeaders.has('User-Agent')) attemptHeaders.set('User-Agent', defaultUserAgent(env))

    const url = new URL(target.toString())
    url.search = params.toString()

    let res: Response
    try {
      res = await fetch(url.toString(), {
        method,
        headers: attemptHeaders,
        body: body && body.byteLength > 0 ? body : undefined,
        redirect: 'manual',
      })
    } catch {
      // Network failure: try the next key.
      continue
    }

    if (!isRetryable(res.status)) {
      executionCtx.waitUntil(rpc.recordRequest(config.slug, res.status, Date.now() - started))
      return new Response(res.body, { status: res.status, headers: res.headers })
    }

    const text = (await res.text().catch(() => '')).slice(0, MAX_ERROR_BODY)
    const classification = classifyRateLimit(res.headers, text)
    const waitMs = clampWait(classification, settings)
    await rpc.markCooldown(pick.id, classification.class, waitMs)
    executionCtx.waitUntil(rpc.recordCooldown(config.slug, maskKey(key.key_value), classification.class, waitMs))

    last = {
      status: res.status,
      headers: new Headers(res.headers),
      body: new TextEncoder().encode(text).buffer as ArrayBuffer,
    }
  }

  if (last) {
    const outHeaders = new Headers(last.headers)
    outHeaders.delete('content-length')
    if (!outHeaders.has('Retry-After')) outHeaders.set('Retry-After', '60')
    return new Response(last.body, { status: last.status, headers: outHeaders })
  }

  return Response.json({ detail: 'All API keys are rate limited, please try again later' }, { status: 429 })
}