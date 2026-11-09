# Production Deployment & Rotation Runbook

This document is the operational contract for **taking a change live on the internet**
and for **rotating the running API Key Rotator**.

Audience: any agent or operator working on this repository or on the production host.
If a step here conflicts with something else you were told, follow this document and
raise the conflict before changing production.

---

## 1. Production topology (do not change these invariants)

```
Internet
  └─ nginx :443 (rotator.rvndha.duckdns.org)
       └─ http://127.0.0.1:8020
            └─ Docker container "api-key-rotator"
                 └─ upstream providers (OpenAI / OpenRouter / Anthropic / Gemini / ...)
```

| Item | Production value |
|---|---|
| Public URL | `https://rotator.rvndha.duckdns.org` |
| nginx upstream | `http://127.0.0.1:8020` |
| Container name | `api-key-rotator` |
| Image | `api-key-rotator:<tag>` (currently date-tagged, e.g. `h1h2h3-20261106-3`) |
| Container port binding | `127.0.0.1:8020:8000` — **loopback only, never `0.0.0.0`** |
| Data volume | `/home/ubuntu/.local/share/api-key-rotator` → `/app/data` |
| SQLite file | `/home/ubuntu/.local/share/api-key-rotator/api_key_rotator.db` |
| Env file | `/home/ubuntu/.local/share/api-key-rotator/rotator.env` |
| Proxy key source | `/home/ubuntu/.hermes/.env` → `HERMES_CUSTOM_APIKEYROTATOR_ONRENDER_COM_API_KEY` |
| Start/restart script | `/home/ubuntu/.local/share/api-key-rotator/run-rotator.sh` |
| Registry | `ghcr.io/arvindg4u/apikeyrotator:<tag>` (+ `latest`) |
| Dev compose | root `docker-compose.yml` — **dev only**, container is `api-key-rotator-dev`, loopback only |

**Production is started by `run-rotator.sh`. Do not start it with `docker run` or
`docker compose` by hand.**

---

## 2. The two rotations

There are two distinct operations people mean by "rotate the rotator". Do not mix them.

1. **Rotate the deployment** — ship a new image/container (new code, dependency, config).
2. **Rotate credentials/keys** — change the admin password, JWT secret, proxy key, or
   upstream provider keys.

Both require a backup first and a verification step after.

---

## 3. Rotate the deployment (ship a change)

### 3.1 Preconditions

```bash
cd /home/ubuntu/APIKeyRotator
git status --short          # must be clean
git log --oneline -1        # know the commit you are shipping
```

CI must be green. A red CI must not be merged or deployed.

### 3.2 Run the local checks

Backend:

```bash
cd backend
go build ./...
go vet ./...
go test ./...
```

Frontend (pnpm v9; see `CLAUDE.md`):

```bash
cd frontend
npm run build
```

### 3.3 Commit and push

Use the commit format from `CLAUDE.md` (`feat:`, `fix:`, `chore:`, ...). Push the
branch or `master` according to the work's approval.

### 3.4 Choose a tag and build one image

Use a date tag; add `-2`, `-3` for a second/third deploy in the same day.

```bash
cd /home/ubuntu/APIKeyRotator
TAG=h1h2h3-$(date +%Y%m%d)          # e.g. h1h2h3-20261109
sudo docker build -t api-key-rotator:$TAG -t api-key-rotator:production .
```

One image contains both backend and the built frontend. Do not build a separate
frontend image for production.

### 3.5 Push the image to GHCR

```bash
gh auth token | sudo docker login ghcr.io -u arvindg4u --password-stdin
sudo docker tag api-key-rotator:$TAG ghcr.io/arvindg4u/apikeyrotator:$TAG
sudo docker tag api-key-rotator:$TAG ghcr.io/arvindg4u/apikeyrotator:latest
sudo docker push ghcr.io/arvindg4u/apikeyrotator:$TAG
sudo docker push ghcr.io/arvindg4u/apikeyrotator:latest
```

The package is private. Pulling it elsewhere needs `docker login ghcr.io`.

### 3.6 Recommended preflight with a copy of the DB

Never test against the live data directory.

```bash
rm -rf /tmp/rotator-preflight-data
cp -a /home/ubuntu/.local/share/api-key-rotator /tmp/rotator-preflight-data

sudo docker rm -f rotator-preflight >/dev/null 2>&1 || true
sudo docker run -d --name rotator-preflight -p 127.0.0.1:18080:8000 \
  -e ADMIN_PASSWORD=preflight-pass \
  -e JWT_SECRET=preflight-secret \
  -e GLOBAL_PROXY_KEYS=preflight-key \
  -v /tmp/rotator-preflight-data:/app/data \
  api-key-rotator:$TAG

curl -fsS http://127.0.0.1:18080/health
# log in with preflight-pass and check:
#   GET /admin/proxy-configs            -> config count
#   GET /admin/proxy-configs/16/models  -> 200 and a model list
```

Remove the preflight container and copied data when done:

```bash
sudo docker rm -f rotator-preflight
rm -rf /tmp/rotator-preflight-data
```

### 3.7 Backup data and script

Do this before every cutover. The SQLite DB uses WAL, so a consistent copy is taken
around a container stop.

```bash
STAMP=$(date +%Y%m%d-%H%M%S)
cp -a /home/ubuntu/.local/share/api-key-rotator \
      "/home/ubuntu/.local/share/api-key-rotator.bak-${STAMP}-pre-${TAG}"
cp -a /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh \
      "/home/ubuntu/.local/share/api-key-rotator/run-rotator.sh.bak-${STAMP}-pre-${TAG}"
```

### 3.8 Point the start script at the new image

Edit `/home/ubuntu/.local/share/api-key-rotator/run-rotator.sh`:

```bash
IMAGE=api-key-rotator:$TAG
```

Confirm it still contains:

```bash
-p 127.0.0.1:8020:8000
--env-file "$ENV_FILE"
-v "$DATA_DIR":/app/data
```

### 3.9 Cut over

```bash
sudo docker stop api-key-rotator
sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh
```

The script removes the old container, creates the new one, and prints
`api-key-rotator started -> https://rotator.rvndha.duckdns.org`.

### 3.10 Verify

```bash
sudo docker ps --filter name=api-key-rotator \
  --format '{{.Image}} | {{.Status}} | {{.Ports}}'
# expect: api-key-rotator:$TAG | Up (healthy) | 127.0.0.1:8020->8000/tcp

curl -fsS http://127.0.0.1:8020/health
curl -fsS https://rotator.rvndha.duckdns.org/health

sudo docker inspect api-key-rotator \
  --format 'Health={{.State.Health.Status}} Ports={{json .HostConfig.PortBindings}} Mounts={{json .Mounts}}'
```

Then verify the data and admin path with the real secrets:

```bash
sudo bash -c '
set -a; . /home/ubuntu/.local/share/api-key-rotator/rotator.env; set +a
user="${ADMIN_USERNAME:-admin}"
token=$(curl -sS -m 5 -X POST http://127.0.0.1:8020/admin/login \
  -H "Content-Type: application/json" \
  --data "$(printf "{\"username\":\"%s\",\"password\":\"%s\"}" "$user" "$ADMIN_PASSWORD")" \
  | sed -n "s/.*\"token\":\"\([^\"]*\)\".*/\1/p")
echo "login:$([ -n "$token" ] && echo ok || echo failed)"
curl -sS -H "Authorization: Bearer $token" http://127.0.0.1:8020/admin/proxy-configs | jq length
'
```

Expected: `login:ok` and the production config count (currently 17).

Also confirm the weak defaults are not accepted:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8020/admin/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}'   # must be 401
```

If the change affects the frontend, also confirm the new asset is served publicly:

```bash
curl -sS https://rotator.rvndha.duckdns.org/ | grep -o 'assets/index-[^"]*\.js'
```

### 3.11 Rollback

Fast rollback is a script edit plus a re-run:

```bash
# In run-rotator.sh:
IMAGE=api-key-rotator:<previous-tag>
sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh
```

The previous image is kept on the host. Restore the data backup only if the failed
deploy changed the database and you need to undo that data change:

```bash
sudo docker stop api-key-rotator
cp -a "/home/ubuntu/.local/share/api-key-rotator.bak-<stamp>-pre-<tag>/." \
      /home/ubuntu/.local/share/api-key-rotator/
sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh
```

---

## 4. Rotate credentials and keys

### 4.1 Admin password / JWT secret

`ADMIN_PASSWORD` and `JWT_SECRET` live in `rotator.env`. Both must be set; never
fall back to the code defaults (`admin123`, `your-secret-key`).

```bash
cp -a /home/ubuntu/.local/share/api-key-rotator/rotator.env \
      /home/ubuntu/.local/share/api-key-rotator/rotator.env.bak-$(date +%Y%m%d-%H%M%S)
# edit rotator.env: ADMIN_PASSWORD=..., JWT_SECRET=...
sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh
```

- Changing `JWT_SECRET` invalidates **all** admin sessions immediately.
- Changing `ADMIN_PASSWORD` forces the next login to use the new value.
- Tell the admin user before rotating; do not commit `rotator.env` to git.

### 4.2 Proxy key (`GLOBAL_PROXY_KEYS`)

The authoritative proxy key for the production script is read from
`/home/ubuntu/.hermes/.env`:

```bash
HERMES_CUSTOM_APIKEYROTATOR_ONRENDER_COM_API_KEY=...
```

`run-rotator.sh` passes it with `-e GLOBAL_PROXY_KEYS="$KEY"`; the value in
`rotator.env` is overridden. To rotate:

1. Generate a new key.
2. Update the consumers first (OpenCode CLI, AdGuard, any other client) or update both
   sides in one maintenance window.
3. Update `/home/ubuntu/.hermes/.env`.
4. Restart with `run-rotator.sh`.
5. Verify the old key is rejected and the new key works through
   `https://rotator.rvndha.duckdns.org/llm/<slug>/...`.

### 4.3 Upstream provider keys

Provider keys are managed per service config. Rotate them through the admin UI or the
API:

- `POST /admin/proxy-configs/:id/keys`
- `POST /admin/proxy-configs/:id/keys/batch`

Back up the DB first (section 3.7). A `429` rotates keys automatically and cools the
rate-limited key; do not bulk-replace keys while an upstream is actively rate-limiting
unless you are also changing the account/quota.

### 4.4 Database backup before data changes

The host has no `sqlite3` CLI, so the reliable path is stop/copy/start:

```bash
sudo docker stop api-key-rotator
cp -a /home/ubuntu/.local/share/api-key-rotator \
      "/home/ubuntu/.local/share/api-key-rotator.bak-$(date +%Y%m%d-%H%M%S)"
sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh
```

---

## 5. Golden rules / lessons from the 2026-11-09 incident

On 2026-11-09 the production container was replaced by a dev-style container that:

- used `./data` in the repo instead of the production data dir (only 1 of 17 configs),
- bound `0.0.0.0:8020` instead of `127.0.0.1:8020`,
- started with `ADMIN_PASSWORD=admin123` / `JWT_SECRET=your-secret-key`.

To prevent a repeat:

1. **Never** run `docker run` or `docker compose` for the production container by hand.
   Use `run-rotator.sh`.
2. **Never** bind production to `0.0.0.0`. It must be `127.0.0.1:8020:8000`.
3. **Never** start a production-like container without `ADMIN_PASSWORD`,
   `JWT_SECRET`, and `GLOBAL_PROXY_KEYS`.
4. **Never** replace the `api-key-rotator` container while pointing `-v` at a different
   directory.
5. The root `docker-compose.yml` is dev-only (`api-key-rotator-dev`, loopback, required
   secrets). If you need a local stack, use it under a different project name and do not
   reuse the production port.
6. After every cutover, check the **image, port binding, mount, config count, key count,
   and public health**.
7. If a dev container has already written a different SQLite DB, **preserve it**
   (e.g. move it to `rogue-local-data-<stamp>`) and inspect for unique keys before
   considering deletion. Do not silently delete data.
8. Never paste production secrets into logs, tickets, or chat.

---

## 6. Deployment record (fill in for each rotation)

```
Date / operator:
Commit:
Image tag:
Data backup:
Script backup:
Preflight result:
Cutover result:
Public health:
Config count / key count:
Rollback tag:
Notes / follow-ups:
```

---

## 7. Quick reference

| Action | Command |
|---|---|
| Local checks | `cd backend && go build ./... && go vet ./... && go test ./...` |
| Frontend build | `cd frontend && npm run build` |
| Build image | `sudo docker build -t api-key-rotator:$TAG -t api-key-rotator:production .` |
| Push image | `gh auth token \| sudo docker login ghcr.io -u arvindg4u --password-stdin && sudo docker tag/push ...` |
| Back up data | `cp -a /home/ubuntu/.local/share/api-key-rotator .../api-key-rotator.bak-<stamp>-pre-<tag>` |
| Change image | edit `IMAGE=` in `/home/ubuntu/.local/share/api-key-rotator/run-rotator.sh` |
| Cut over | `sudo docker stop api-key-rotator && sudo bash /home/ubuntu/.local/share/api-key-rotator/run-rotator.sh` |
| Health | `curl -fsS http://127.0.0.1:8020/health && curl -fsS https://rotator.rvndha.duckdns.org/health` |
| Verify bind | `sudo docker inspect api-key-rotator --format '{{json .HostConfig.PortBindings}}'` |
| Verify data | `sudo bash -c '. .../rotator.env; ... login ...; curl .../admin/proxy-configs \| jq length'` |
| Roll back | set `IMAGE` to the previous tag and run `run-rotator.sh` again |
