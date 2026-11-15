# API Key Rotator — Cloudflare Workers deployment

A free-plan, Cloudflare-native reimplementation of the Go backend:

- **Worker** (`src/`) — router, admin JWT auth, proxy passthrough, key rotation.
- **D1** — `proxy_configs` + `api_keys` (replaces SQLite).
- **Durable Object `Rotator`** (one per config slug) — round-robin counter, per-key
  cooldowns, 429 classification, per-config telemetry.
- **Static assets** — the existing Vue admin UI from `../frontend/dist`.

The local Docker deployment is untouched; this is a parallel deployment with its own
D1 database and its own copy of the proxy configs/keys.

## Free-plan constraints this design respects

- 10 ms CPU per request → the proxy **buffers the body but never parses it**; no format
  conversion (openai ↔ anthropic ↔ gemini) in this version.
- 50 subrequests per invocation → at most 45 key attempts per request.
- 100,000 requests/day per account.
- D1: 500 MB per database, 2 MB per row.
- Egress uses Cloudflare's shared IPs (not a dedicated IP).

## First-time setup

```bash
cd cloudflare
npm install

# 1. Log in and create the D1 database
npx wrangler login
npx wrangler d1 create apikeyrotator
# copy the returned database_id into wrangler.jsonc (d1_databases[0].database_id)

# 2. Apply the D1 schema
npx wrangler d1 migrations apply DB --remote

# 3. Set secrets (never commit these)
npx wrangler secret put ADMIN_USERNAME
npx wrangler secret put ADMIN_PASSWORD
npx wrangler secret put JWT_SECRET
npx wrangler secret put GLOBAL_PROXY_KEYS

# 4. Build the existing Vue admin UI, then deploy
cd ../frontend && npm install && npm run build && cd ../cloudflare
npx wrangler deploy
```

The Worker is served at `https://<name>.<your-subdomain>.workers.dev`.

## Auto-deploy on push (Workers Builds)

The Worker project is already connected to this GitHub repository. In
**Workers & Pages → api-key-rotator → Settings → Builds**, set:

| Setting | Value |
|---|---|
| Branch | `master` |
| Root directory | *(repository root)* |
| Build command | `cd frontend && npm install && npm run build` |
| Deploy command | `npx wrangler d1 migrations apply DB --remote && npx wrangler deploy --config cloudflare/wrangler.jsonc` |
| Watch paths | `cloudflare/**`, `frontend/**` |

`assets.directory` is `../frontend/dist` relative to `wrangler.jsonc`, so the deploy
command must run from the repository root (or keep the root directory blank and let
Workers Builds run commands there).

Secrets are configured once in **Settings → Variables and Secrets** (or via
`wrangler secret put`); they survive deploys.

## Importing existing configs and keys

The host has no `sqlite3` CLI. With Python available, export from the local DB:

```python
import sqlite3
con = sqlite3.connect('/home/ubuntu/.local/share/api-key-rotator/api_key_rotator.db')
with open('seed.sql', 'w') as out:
    for row in con.execute('SELECT id,name,slug,config_type,is_active,target_url,target_base_url,method,api_format,output_format,api_key_location,api_key_name,extra_headers,created_at,updated_at FROM proxy_configs'):
        out.write("INSERT OR IGNORE INTO proxy_configs (id,name,slug,config_type,is_active,target_url,target_base_url,method,api_format,output_format,api_key_location,api_key_name,extra_headers,created_at,updated_at) VALUES (" + ','.join('NULL' if v is None else repr(v) for v in row) + ");\n")
    for row in con.execute('SELECT id,proxy_config_id,key_value,is_active FROM api_keys'):
        out.write("INSERT OR IGNORE INTO api_keys (id,proxy_config_id,key_value,is_active) VALUES (" + ','.join('NULL' if v is None else repr(v) for v in row) + ");\n")
```

Then:

```bash
npx wrangler d1 execute DB --remote --file=seed.sql
rm seed.sql   # contains real API keys — never commit it
```

Alternatively, add the service and keys through the admin UI.

## What is intentionally different from the Go backend

- No `openai ↔ anthropic ↔ gemini` request/response conversion (CPU budget). OpenAI-
  compatible passthrough covers tokenharbor, Kilo, OpenRouter, etc.
- Rotation/cooldown state lives in a Durable Object instead of the memory cache, so it
  is consistent and survives isolate restarts.
- Status telemetry is per-config in the Durable Object and aggregated by the admin API.