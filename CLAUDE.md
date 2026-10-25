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
go build ./...            # what CI runs; `go build -o ./api-key-rotator .` emits the binary
go vet ./...
go test ./...
go test -run TestGenerateToken ./internal/auth/    # one test / one package
go test -cover ./...
```

The sqlite driver (`mattn/go-sqlite3`) needs CGO, so leave `CGO_ENABLED` at its default and keep a C compiler on PATH — CI depends on the ubuntu runner's gcc.

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

Tests: `cd backend && go test ./...` — unit tests live next to the packages they cover (`internal/services/` covers rotation, failover and upstream headers; `internal/handlers/` covers the LLM proxy including WAF retry; `internal/converters/formats/{openai,gemini}/` cover format conversion; `internal/auth/` + `internal/middleware/` cover admin tokens). No lint config exists — `go vet` is the only static check.

CI (`.github/workflows/ci.yml`) runs on every push to `master` and every PR: backend `build` + `vet` + `test` (Go 1.22), then frontend `pnpm install` + `pnpm run build` (pnpm 9, Node 20). It exists because a compile error once reached `master` and was only caught by the Render docker build; a red CI must not be merged.

## Architecture

Entry: `backend/main.go` — `godotenv.Load` → `logger.Setup` → `config.Load()` → `config.NewInfrastructureFactory` → `dbRepo.Migrate()` (or `Reset()` when `RESET_DB_TABLES=true`) → `router.Setup(...)` → `r.Run(":"+cfg.Port)`.

- `internal/config/` (`config.go`, `factory.go`): env-driven. Port resolution is `PORT` > `BACKEND_PORT` > `8000` (platform injects `PORT`; root Dockerfile bakes `BACKEND_PORT=8000` which must not win). DB/cache types auto-detected; `DATABASE_URL`/`REDIS_URL` take priority over split `DB_*`/`REDIS_*` vars. Copy `.env.example.en` (or `.cn`) for the full variable list; required secrets are `ADMIN_PASSWORD`, `JWT_SECRET`, `GLOBAL_PROXY_KEYS`.
- `internal/infrastructure/` — interface abstraction: `database/interface.go` (Repository) with `sqlite/` + `mysql/` impls; `cache/interface.go` with `memory/` + `redis/` impls. New backends (e.g. Postgres) follow the same pattern: add impl package, wire into `config/factory.go`.
- `internal/router/router.go` (Gin, DebugMode): `GET /health` (Render/Docker probe) → static `./static/index.html` + `/assets` → every other top-level file in `./static` registered as a root static file (PWA `sw.js`/manifest/icons must be served from the site root; skipped silently when `./static` is absent, e.g. plain `go run`) → `NoRoute` SPA fallback (returns index.html for non-`/admin|/proxy|/llm` paths, 404 JSON for those) → `/admin/*` management, `/proxy/*slug` generic proxy, `/llm/:slug/*action` LLM proxy.
- **Two distinct auth models — do not conflate them:**
  - **Admin** (`/admin/*`): `POST /login` and `GET /app-config` are public; *every other* admin route sits in a nested `adminProtected` group behind `middleware.RequireAdmin(cfg.JWTSecret)`. Login compares credentials with `auth.ConstantTimeEquals` (no prefix-timing leak) and returns a 24h HS256 JWT. `internal/auth/token.go` implements JWT with the standard library only (`crypto/hmac` + `encoding/base64`) — **do not add a JWT dependency**. Accepts `Authorization: Bearer <token>` or `X-Admin-Token`; sets `admin_username` in the Gin context. The frontend stores it as `authToken` in `localStorage`, attaches the header in `api/index.js`, and that interceptor redirects to `/login` on any `401`. Adding a new admin route means adding it inside `adminProtected`, not to `adminAPI` directly.
  - **Proxy** (`/proxy`, `/llm`): no session — the caller authenticates with a `GLOBAL_PROXY_KEYS` value, and the key is extracted from several places (`BaseLLMAdapter`/`proxy.go` accept `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, `x-anthropic-api-key`, or a `?key=` query param) so that native LLM SDKs work unmodified.
  These interfaces can read/write the upstream key pools, which is why they are not merely IP-allowlisted.
- Request flow: `handlers/proxy.go` + `handlers/llm_proxy.go` → `services/proxy_handler.go` (rotation/failover) → `adapters/` (`base_adapter.go` owns proxy-key extraction/validation; `openai_adapter.go`, `anthropic_adapter.go`, `gemini_adapter.go`) for LLM dialects. `handlers/management.go` = CRUD for proxy configs + key pools. Supporting: `models/`, `dto/`, `converters/`, `middleware/`, `logger/`, `utils/`.
- **Format conversion** (`converters/`, `proxy_configs.api_format` / `output_format`): each dialect registers itself from `init()` into a global registry (`converters/formats/registry.go`), imported for side effects in `converters/converter.go`. Registered names: `openai`, `anthropic`, `gemini`, `openai_responses`. `NeedsConversion` decides whether to translate; every handler parses to a shared "universal" representation and the target handler rebuilds from it. Conversion applies to both request and response, streaming included. Adding a dialect = new `formats/<name>/` package with `ParseRequest`/`BuildRequest` (+ streaming) that calls `RegisterFormat`.
- **Upstream request headers** (`services.ApplyUpstreamHeaders`, `UPSTREAM_USER_AGENT`, `proxy_configs.extra_headers`): the client's `User-Agent` is forwarded as-is (`utils.FilterRequestHeaders` must not strip it — it is not a hop-by-hop header). When the client sends none — or sends an *empty* one, which Go drops entirely — the default `UPSTREAM_USER_AGENT` applies, because Go's own default (`Go-http-client/1.1`) is blocked by many WAFs/CDNs with a `403` before the request reaches the model. Per-config `extra_headers` (JSON object or `Name: value` lines) override everything and are the fix for "upstream 403 Blocked".
- **WAF block-page retry** (`services.ApplySafeUserAgent`, `WAF_RETRY_USER_AGENT`, `UPSTREAM_WAF_RETRY`): a `403`/`401` whose body is not JSON (`services.LooksLikeBlockPage`) is a WAF/CDN block, not a bad key — rotating keys cannot help. The handlers (`llm_proxy.go`, `proxy.go`) therefore retry the **same** key once with `WAF_RETRY_USER_AGENT` (a plain browser UA) before falling back to key rotation; the retry happens at most once per request and the new UA stays for the remaining key attempts. Disable with `UPSTREAM_WAF_RETRY=false` or an empty `WAF_RETRY_USER_AGENT`.
- **Key rotation / 429 handling** (`services/key_rotator.go`, `KEY_COOLDOWN_SECONDS`): the upstream key is injected **once per attempt** by `services.InjectUpstreamKey`, and *only* there — adapters must not inject keys themselves, or the first attempt and the retry land in different places. `KeyRotator.Next` takes a `tried` set so one request never tries the same key twice (a shared cache counter alone is not enough: concurrent requests steal each other's increments and retries end up reusing the key that just 429'd). A 429 puts that key in cooldown (`MarkRateLimited`, honours upstream `Retry-After`/Gemini `retryDelay`), which *deprioritises* it but never disables it: when every key is cooling, the one that recovers soonest is still used. When all keys fail, the last buffered upstream response is written back plus a `Retry-After` header. `KEY_COOLDOWN_SECONDS=0` disables cooldown.
- Frontend `src/`: `views/` (Layout, Dashboard, Login), `components/` (KeyManager, LangSwitcher), `api/index.js` (axios), `router/index.js`, `i18n.js` + `locales/{en.json,zh-CN.json}`, `@` → `src`. Dev proxy rules live in `vite.config.js`. Built with `vite-plugin-pwa` (`registerType: 'autoUpdate'`) — the service worker is generated into the build output, which is the reason the router registers root-level `./static` files one by one. UI strings must be added to both `locales/en.json` and `locales/zh-CN.json`.

## Deployment gotchas

- `entrypoint.sh`: when `R2_ACCESS_KEY_ID`/`R2_SECRET_ACCESS_KEY` are set, runs under Litestream (SQLite → R2 replica, `sync-interval: 10s`); otherwise runs the binary directly. Render path uses `Dockerfile.render` + `render.yaml` blueprint (disk mount `/var/data`, `DATABASE_PATH=/var/data/api_key_rotator.db`).
- **Never `touch` or bake an empty DB file into the image** — Litestream treats an existing empty file as real data, skips `restore-if-db-not-exists`, and replicates the empty DB over the R2 backup (2026-09-06 incident; see `entrypoint.sh` which deletes zero-byte DBs before restore).
- **`JWT_SECRET`, `ADMIN_PASSWORD` and `GLOBAL_PROXY_KEYS` all fall back to well-known published defaults** (`your-secret-key`, `admin123`, `your-global-proxy-key`) when unset. That was low-stakes while `JWT_SECRET` was decorative, but it now signs the admin session tokens, and `ADMIN_PASSWORD`+`JWT_SECRET` together are a full admin compromise on any deployment that forgot them. Set all three explicitly; consider failing startup on defaults rather than warning.
- Backend commit messages in this repo are terse lowercase; comments in code are largely Chinese.
- `README.md` / `README_CN.md` are maintained in parallel (as are `backend/` and `frontend/` READMEs) — a change to documented behaviour or an env var should land in both. `README.md` carries the user-facing env-var table (`KEY_COOLDOWN_SECONDS`, `UPSTREAM_USER_AGENT`, `WAF_RETRY_USER_AGENT`, …); `CLAUDE.md` is not the place for it.
