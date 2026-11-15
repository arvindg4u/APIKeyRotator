-- D1 schema. Mirrors the production SQLite tables (proxy_configs + api_keys).
-- Booleans are stored as INTEGER 0/1 and converted in src/lib/db.ts.

CREATE TABLE IF NOT EXISTS proxy_configs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  slug TEXT NOT NULL UNIQUE,
  config_type TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  target_url TEXT,
  target_base_url TEXT,
  method TEXT,
  api_format TEXT DEFAULT 'openai_compatible',
  output_format TEXT DEFAULT 'none',
  api_key_location TEXT,
  api_key_name TEXT,
  extra_headers TEXT,
  created_at TEXT,
  updated_at TEXT
);

CREATE TABLE IF NOT EXISTS api_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  proxy_config_id INTEGER NOT NULL,
  key_value TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_api_keys_config ON api_keys (proxy_config_id);