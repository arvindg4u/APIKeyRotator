export interface Env {
  DB: D1Database
  ROTATOR: DurableObjectNamespace
  ASSETS: Fetcher

  ADMIN_USERNAME?: string
  ADMIN_PASSWORD?: string
  JWT_SECRET?: string
  GLOBAL_PROXY_KEYS?: string
  PROXY_PUBLIC_BASE_URL?: string

  UPSTREAM_USER_AGENT?: string
  KEY_COOLDOWN_SECONDS?: string
  KEY_MAX_COOLDOWN_SECONDS?: string
  FREE_TIER_COOLDOWN_SECONDS?: string
  FREE_TIER_MAX_COOLDOWN_SECONDS?: string
}

export interface APIKey {
  id: number
  proxy_config_id: number
  key_value: string
  is_active: boolean
}

// Matches the JSON shape the existing Vue admin UI expects (snake_case).
export interface ProxyConfig {
  id: number
  name: string
  slug: string
  config_type: string
  is_active: boolean
  target_url?: string | null
  target_base_url?: string | null
  method?: string | null
  api_format?: string | null
  output_format?: string | null
  api_key_location?: string | null
  api_key_name?: string | null
  extra_headers?: string | null
  created_at?: string | null
  updated_at?: string | null
  api_keys?: APIKey[]
}

export interface ProxyConfigInput {
  name: string
  slug: string
  config_type: string
  is_active?: boolean
  target_url?: string | null
  target_base_url?: string | null
  method?: string | null
  api_format?: string | null
  output_format?: string | null
  api_key_location?: string | null
  api_key_name?: string | null
  extra_headers?: string | null
}

export interface RotatorSnapshot {
  total: number
  statuses: Record<string, number>
  rpm: number[]
  cooling: number
}

export interface StoredEvent {
  id: number
  time: number
  level: string
  kind: string
  slug?: string
  method?: string
  path?: string
  status?: number
  latencyMs?: number
  keyMasked?: string
  class?: string
  message?: string
}

export interface StoredLog {
  id: number
  time: number
  level: string
  message: string
}

// RPC surface of the Rotator Durable Object.
export interface RotatorRpc {
  next(activeIds: number[], triedIds: number[]): Promise<{ id: number; waitMs: number } | null>
  markCooldown(id: number, className: string, waitMs: number): Promise<void>
  recordRequest(slug: string, status: number, latencyMs: number): Promise<void>
  recordCooldown(slug: string, keyMasked: string, className: string, waitMs: number): Promise<void>
  snapshot(): Promise<RotatorSnapshot>
  eventsSince(sinceId: number, limit: number): Promise<StoredEvent[]>
  logsSince(sinceId: number, limit: number): Promise<StoredLog[]>
}
