import type { APIKey, ProxyConfig, ProxyConfigInput } from './types'

function toBool(value: unknown): boolean {
  return value === 1 || value === true || value === '1'
}

function rowToConfig(row: Record<string, unknown>): ProxyConfig {
  return {
    id: Number(row.id),
    name: String(row.name),
    slug: String(row.slug),
    config_type: String(row.config_type),
    is_active: toBool(row.is_active),
    target_url: (row.target_url as string) ?? null,
    target_base_url: (row.target_base_url as string) ?? null,
    method: (row.method as string) ?? null,
    api_format: (row.api_format as string) ?? null,
    output_format: (row.output_format as string) ?? null,
    api_key_location: (row.api_key_location as string) ?? null,
    api_key_name: (row.api_key_name as string) ?? null,
    extra_headers: (row.extra_headers as string) ?? null,
    created_at: (row.created_at as string) ?? null,
    updated_at: (row.updated_at as string) ?? null,
  }
}

function rowToKey(row: Record<string, unknown>): APIKey {
  return {
    id: Number(row.id),
    proxy_config_id: Number(row.proxy_config_id),
    key_value: String(row.key_value),
    is_active: toBool(row.is_active),
  }
}

async function attachKeys(db: D1Database, configs: ProxyConfig[]): Promise<ProxyConfig[]> {
  for (const config of configs) {
    const keys = await db.prepare('SELECT * FROM api_keys WHERE proxy_config_id = ? ORDER BY id').bind(config.id).all()
    config.api_keys = (keys.results ?? []).map(rowToKey)
  }
  return configs
}

export async function listConfigs(db: D1Database): Promise<ProxyConfig[]> {
  const res = await db.prepare('SELECT * FROM proxy_configs ORDER BY id').all()
  return attachKeys(db, (res.results ?? []).map(rowToConfig))
}

export async function getConfigById(db: D1Database, id: number): Promise<ProxyConfig | null> {
  const row = await db.prepare('SELECT * FROM proxy_configs WHERE id = ?').bind(id).first()
  if (!row) return null
  const config = rowToConfig(row as Record<string, unknown>)
  const keys = await db.prepare('SELECT * FROM api_keys WHERE proxy_config_id = ? ORDER BY id').bind(id).all()
  config.api_keys = (keys.results ?? []).map(rowToKey)
  return config
}

export async function getConfigBySlug(db: D1Database, slug: string): Promise<ProxyConfig | null> {
  const row = await db.prepare('SELECT * FROM proxy_configs WHERE slug = ?').bind(slug).first()
  if (!row) return null
  const config = rowToConfig(row as Record<string, unknown>)
  const keys = await db.prepare('SELECT * FROM api_keys WHERE proxy_config_id = ? ORDER BY id').bind(config.id).all()
  config.api_keys = (keys.results ?? []).map(rowToKey)
  return config
}

export async function createConfig(db: D1Database, input: ProxyConfigInput): Promise<ProxyConfig> {
  const now = new Date().toISOString()
  const res = await db
    .prepare(
      `INSERT INTO proxy_configs
       (name, slug, config_type, is_active, target_url, target_base_url, method, api_format, output_format, api_key_location, api_key_name, extra_headers, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    )
    .bind(
      input.name,
      input.slug,
      input.config_type,
      input.is_active ? 1 : 0,
      input.target_url ?? null,
      input.target_base_url ?? null,
      input.method ?? null,
      input.api_format ?? 'openai_compatible',
      input.output_format ?? 'none',
      input.api_key_location ?? null,
      input.api_key_name ?? null,
      input.extra_headers ?? null,
      now,
      now,
    )
    .run()
  return (await getConfigById(db, Number(res.meta.last_row_id)))!
}

export async function updateConfig(db: D1Database, id: number, input: ProxyConfigInput): Promise<ProxyConfig | null> {
  const now = new Date().toISOString()
  await db
    .prepare(
      `UPDATE proxy_configs SET
         name = ?, slug = ?, config_type = ?, is_active = ?, target_url = ?, target_base_url = ?,
         method = ?, api_format = ?, output_format = ?, api_key_location = ?, api_key_name = ?,
         extra_headers = ?, updated_at = ?
       WHERE id = ?`,
    )
    .bind(
      input.name,
      input.slug,
      input.config_type,
      input.is_active ? 1 : 0,
      input.target_url ?? null,
      input.target_base_url ?? null,
      input.method ?? null,
      input.api_format ?? 'openai_compatible',
      input.output_format ?? 'none',
      input.api_key_location ?? null,
      input.api_key_name ?? null,
      input.extra_headers ?? null,
      now,
      id,
    )
    .run()
  return getConfigById(db, id)
}

export async function updateConfigStatus(db: D1Database, id: number, isActive: boolean): Promise<void> {
  await db.prepare('UPDATE proxy_configs SET is_active = ?, updated_at = ? WHERE id = ?').bind(isActive ? 1 : 0, new Date().toISOString(), id).run()
}

export async function deleteConfig(db: D1Database, id: number): Promise<void> {
  await db.batch([
    db.prepare('DELETE FROM api_keys WHERE proxy_config_id = ?').bind(id),
    db.prepare('DELETE FROM proxy_configs WHERE id = ?').bind(id),
  ])
}

export async function listKeys(db: D1Database, configId: number): Promise<APIKey[]> {
  const res = await db.prepare('SELECT * FROM api_keys WHERE proxy_config_id = ? ORDER BY id').bind(configId).all()
  return (res.results ?? []).map(rowToKey)
}

export async function listActiveKeys(db: D1Database, configId: number): Promise<APIKey[]> {
  const res = await db.prepare('SELECT * FROM api_keys WHERE proxy_config_id = ? AND is_active = 1 ORDER BY id').bind(configId).all()
  return (res.results ?? []).map(rowToKey)
}

export async function createKey(db: D1Database, configId: number, keyValue: string, isActive: boolean): Promise<APIKey> {
  const res = await db
    .prepare('INSERT INTO api_keys (proxy_config_id, key_value, is_active) VALUES (?, ?, ?)')
    .bind(configId, keyValue, isActive ? 1 : 0)
    .run()
  return { id: Number(res.meta.last_row_id), proxy_config_id: configId, key_value: keyValue, is_active: isActive }
}

// Batch insert + dedupe against the keys already stored for this config.
// Returns the same counters the Go response used.
export async function batchCreateKeys(
  db: D1Database,
  configId: number,
  values: string[],
  isActive: boolean,
): Promise<{ success_count: number; failed_count: number; skipped_count: number; failed_keys: string[] }> {
  const existing = new Set((await listKeys(db, configId)).map((k) => k.key_value))
  let success = 0
  let skipped = 0
  const failed: string[] = []
  const statements: D1PreparedStatement[] = []

  for (const raw of values) {
    const value = raw.trim()
    if (!value) {
      continue
    }
    if (existing.has(value)) {
      skipped++
      continue
    }
    existing.add(value)
    statements.push(db.prepare('INSERT INTO api_keys (proxy_config_id, key_value, is_active) VALUES (?, ?, ?)').bind(configId, value, isActive ? 1 : 0))
    success++
  }

  if (statements.length) {
    await db.batch(statements)
  }

  return { success_count: success, failed_count: failed.length, skipped_count: skipped, failed_keys: failed }
}

export async function clearKeys(db: D1Database, configId: number): Promise<number> {
  const res = await db.prepare('DELETE FROM api_keys WHERE proxy_config_id = ?').bind(configId).run()
  return res.meta.changes ?? 0
}

export async function updateKeyStatus(db: D1Database, keyId: number, isActive: boolean): Promise<void> {
  await db.prepare('UPDATE api_keys SET is_active = ? WHERE id = ?').bind(isActive ? 1 : 0, keyId).run()
}

export async function deleteKey(db: D1Database, keyId: number): Promise<void> {
  await db.prepare('DELETE FROM api_keys WHERE id = ?').bind(keyId).run()
}
