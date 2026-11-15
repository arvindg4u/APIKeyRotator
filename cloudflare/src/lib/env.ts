import type { Env, RotatorRpc } from './types'

export function proxyKeys(env: Env): string[] {
  return (env.GLOBAL_PROXY_KEYS || '')
    .split(',')
    .map((k) => k.trim())
    .filter(Boolean)
}

export function cooldownSettings(env: Env) {
  return {
    transient: Number(env.KEY_COOLDOWN_SECONDS ?? 60) || 60,
    transientMax: Number(env.KEY_MAX_COOLDOWN_SECONDS ?? 1800) || 1800,
    freeTier: Number(env.FREE_TIER_COOLDOWN_SECONDS ?? 43200) || 43200,
    freeTierMax: Number(env.FREE_TIER_MAX_COOLDOWN_SECONDS ?? 604800) || 604800,
  }
}

export function defaultUserAgent(env: Env): string {
  return (
    env.UPSTREAM_USER_AGENT ||
    'Mozilla/5.0 (compatible; APIKeyRotator/1.0; +https://github.com/arvindg4u/APIKeyRotator)'
  )
}

export function rotator(env: Env, slug: string): RotatorRpc {
  return env.ROTATOR.get(env.ROTATOR.idFromName(slug)) as unknown as RotatorRpc
}
