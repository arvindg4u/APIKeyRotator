# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project: API Key Rotator

Lightweight API key pool + proxy service. Go/Gin backend (`backend/`) serves both the REST/LLM proxy API and the built Vue 3 frontend from `./static`. Vue 3 + Element Plus SPA (`frontend/`). Two Docker builds: lightweight (SQLite + memory cache, root `Dockerfile`) and enterprise (`Dockerfile.enterprise`, MySQL + Redis).

## Commands

Backend (in `backend/`, Go 1.21+, Docker uses 1.22):
```bash
cd backend
go mod download
go run main.go            # serves on $PORT, else $BACKEND_PORT, else 8000
go build -o ./api-key-rotator .
go test ./...             # no _test.go files exist currently
go test -cover ./...
```

Frontend (in `frontend/`, **pnpm v9 only** — v10+ breaks postinstall for esbuild/vue-demi):
```bash
npm install -g pnpm@9
pnpm install
pnpm run dev               # vite on :5173, proxies /admin /llm /api to $VITE_API_TARGET_URL or localhost:8000
pnpm run build             # outputs dist/ (Docker copies to ./static)
pnpm run preview
```

Docker:
```bash
docker build -t api-key-rotator .                                        # lightweight
docker build -f Dockerfile.enterprise -t api-key-rotator:enterprise .   # enterprise
docker-compose up -d                                                     # lightweight
docker-compose -f docker-compose.enterprise.yml up -d                    # enterprise
```

No lint config exists. No test files exist (backend README references `go test`, but nothing to run).

## Architecture

Entry: `backend/main.go` — `godotenv.Load` → `logger.Setup` → `config.Load()` → `config.NewInfrastructureFactory` → `dbRepo.Migrate()` (or `Reset()` when `RESET_DB_TABLES=true`) → `router.Setup(...)` → `r.Run(":"+cfg.Port)`.

- `internal/config/` (`config.go`, `factory.go`): env-driven. Port resolution is `PORT` > `BACKEND_PORT` > `8000` (platform injects `PORT`; root Dockerfile bakes `BACKEND_PORT=8000` which must not win). DB/cache types auto-detected; `DATABASE_URL`/`REDIS_URL` take priority over split `DB_*`/`REDIS_*` vars. Copy `.env.example.en` (or `.cn`) for the full variable list; required secrets are `ADMIN_PASSWORD`, `JWT_SECRET`, `GLOBAL_PROXY_KEYS`.
- `internal/infrastructure/` — interface abstraction: `database/interface.go` (Repository) with `sqlite/` + `mysql/` impls; `cache/interface.go` with `memory/` + `redis/` impls. New backends (e.g. Postgres) follow the same pattern: add impl package, wire into `config/factory.go`.
- `internal/router/router.go` (Gin, DebugMode): `GET /health` (Render/Docker probe) → static `./static/index.html` + `/assets` → `NoRoute` SPA fallback (returns index.html for non-`/admin|/proxy|/llm` paths) → `/admin/*` management (JWT), `/proxy/*slug` generic proxy, `/llm/:slug/*action` LLM proxy (both key on `GLOBAL_PROXY_KEYS` bearer).
- Request flow: `handlers/proxy.go` + `handlers/llm_proxy.go` → `services/proxy_handler.go` (rotation/failover) → `adapters/` (`base_adapter.go`, `openai_adapter.go`, `anthropic_adapter.go`, `gemini_adapter.go`) for LLM dialects. `handlers/management.go` = CRUD for proxy configs + key pools. Supporting: `models/`, `dto/`, `converters/`, `middleware/cors.go`, `logger/`, `utils/`.
- Frontend `src/`: `views/` (Layout, Dashboard, Login), `components/` (KeyManager, LangSwitcher), `api/index.js` (axios), `router/index.js`, `i18n.js` + `locales/{en.json,zh-CN.json}`, `@` → `src`. Dev proxy rules live in `vite.config.js`.

## Deployment gotchas

- `entrypoint.sh`: when `R2_ACCESS_KEY_ID`/`R2_SECRET_ACCESS_KEY` are set, runs under Litestream (SQLite → R2 replica, `sync-interval: 10s`); otherwise runs the binary directly. Render path uses `Dockerfile.render` + `render.yaml` blueprint (disk mount `/var/data`, `DATABASE_PATH=/var/data/api_key_rotator.db`).
- **Never `touch` or bake an empty DB file into the image** — Litestream treats an existing empty file as real data, skips `restore-if-db-not-exists`, and replicates the empty DB over the R2 backup (2026-09-06 incident; see `entrypoint.sh` which deletes zero-byte DBs before restore).
- Backend commit messages in this repo are terse lowercase; comments in code are largely Chinese.
