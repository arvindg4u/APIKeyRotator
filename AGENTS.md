# AGENTS.md — how we work in this repo

API Key Rotator: a self-hosted gateway that fronts upstream LLM/REST providers and rotates a
pool of API keys. Two parts in one repo:

- `backend/` — Go 1.21 module `api-key-rotator/backend`, Gin + GORM.
- `frontend/` — Vue 3 + Vite + Element Plus SPA. It is built **into the backend image** and
  served by Go from `./static` (there is no separate production frontend image).

## Read these first — they are the source of truth

- `CLAUDE.md` — architecture, invariants, and hard-won gotchas.
- `DEPLOYMENT.md` — the production contract. It overrides other instructions for any deploy/rotate.

IMPORTANT: OpenCode loads this `AGENTS.md` and does **not** auto-load `CLAUDE.md`
(`AGENTS.md` wins when both exist). Before non-trivial work, read `CLAUDE.md` yourself; for
anything touching production, read `DEPLOYMENT.md`.

## Commands

Backend (run from `backend/`):

```bash
go build ./... && go vet ./... && go test ./...
go test ./internal/services -run TestName -v   # single package / single test
```

CGO must stay ON (the SQLite driver needs it). CI (`.github/workflows/ci.yml`) runs exactly
`go build` → `go vet` → `go test` on Go 1.22.

Frontend (run from `frontend/`):

```bash
npm run build                     # works on this host (pnpm is not installed here)
pnpm install && pnpm run build    # CI uses pnpm v9 — v10 breaks esbuild/vue-demi postinstall
```

There is no frontend test runner; `npm run build` is the check. Order for a change:
backend build+vet+test, then frontend build.

## Architecture notes an agent would otherwise miss

- Entry: `backend/main.go` → `logger.Setup` → `config.Load` → infra factory → migrate →
  `router.Setup` → `r.Run`. `RESET_DB_TABLES=true` drops all tables.
- `internal/infrastructure/` is interface-based: `database/` (sqlite, mysql) and `cache/`
  (memory, redis) adapters selected in `config/factory.go`. Production runs sqlite + memory
  ("lightweight"); the memory cache means cooldowns/counters reset on restart.
- **Two auth models — never conflate them.** `/admin/*` is protected by a 24h HS256 JWT
  (`middleware.RequireAdmin`); `/proxy` and `/llm` use the shared `GLOBAL_PROXY_KEYS`. New admin
  routes must be added inside the `adminProtected` group in `router.go`, not to `adminAPI`.
- JWT is implemented with the standard library in `internal/auth/token.go` — do not add a JWT
  dependency.
- Key rotation (`services/key_rotator.go`): the upstream key is injected **once per attempt** by
  `InjectUpstreamKey` (adapters must not inject it). `Next(tried)` never repeats a key within one
  request. A 429 only deprioritises a key (it is not disabled). `ClassifyRateLimit` separates
  transient 429s from `free_tier_limit_reached`, which gets a long cooldown to the parsed reset
  time. When all keys fail, the last buffered upstream response is replayed with `Retry-After`.
- Frontend UI strings must be added to **both** `locales/en.json` and `locales/zh-CN.json`.
- Status page (`internal/telemetry/`, `handlers/status.go`): counters are in-memory and reset on
  restart. The live feed is SSE consumed with **fetch + ReadableStream** (`api/stream.js`), not
  `EventSource` — native `EventSource` cannot send the admin `Authorization` header, and a token
  in the query string would leak into nginx/proxy logs. Through nginx the stream route needs
  `proxy_buffering off`.
- Egress (`internal/egress/`, `EGRESS_*`) is off by default and deliberately ignores a global
  `HTTP_PROXY`/`HTTPS_PROXY`.

## Production — do not improvise

- Ship = build **one** image (backend + built frontend baked in), push to GHCR, set `IMAGE=` in
  `/home/ubuntu/.local/share/api-key-rotator/run-rotator.sh`, then run that script. Full procedure:
  `DEPLOYMENT.md`.
- Invariants: container name `api-key-rotator`, bind **`127.0.0.1:8020:8000`** (loopback only),
  data dir `/home/ubuntu/.local/share/api-key-rotator`, env `rotator.env`, proxy key from
  `/home/ubuntu/.hermes/.env`. Public entry is host nginx
  (`/etc/nginx/sites-available/apikeyrotator-ipv6`) → `127.0.0.1:8020`.
- **Never** start production with `docker run`/`docker compose`, and never bind `0.0.0.0`.
  `docker-compose.yml` is dev-only (`api-key-rotator-dev`, loopback).
- Set `ADMIN_PASSWORD`, `JWT_SECRET`, `GLOBAL_PROXY_KEYS` explicitly — each falls back to a weak
  published default otherwise.
- Back up the data dir **and** `run-rotator.sh` before every cutover. Never bake an empty DB into
  the image (Litestream would replicate it over the R2 backup).

## Conventions

- Commits: Conventional Commits (`feat:`, `fix:`, `chore:`); backend messages are terse
  lowercase. Code comments are largely Chinese.
- `README.md` / `README_CN.md` (and the per-package READMEs) are kept in sync; any behaviour or
  env-var change belongs in both.
