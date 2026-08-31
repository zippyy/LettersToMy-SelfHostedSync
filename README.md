# Letters to My — Self-Hosted Sync Server

Docker-based companion server for
[Letters to My](https://github.com/zippyy/LettersToMy). Provides **backup
storage, attachment storage, and cross-platform collaboration** for the
Apple clients and any other client that speaks the same API.

> **What this server is NOT:** a live cross-platform synchronization engine.
> The `/sync` endpoints store raw platform database snapshots (Core Data
> SQLite on iOS, Room SQLite on Android) as backup artifacts. A Core Data
> database is not an Android Room database; the client never pulls another
> platform's snapshot into its live store. Logical cross-platform record
> sync is future work. The UI on the client does not expose snapshot sync.

## What it does

| Capability | Description |
|------------|-------------|
| **Backup storage** | Opaque encrypted `.letterstomy` archives on `/backup`. The server never sees the passphrase or plaintext — archives are encrypted on the client. |
| **Attachment storage** | Upload/download/delete media blobs on `/attachment` (byte-identical round trip). |
| **Invitations** | Create, look up, accept, revoke cross-platform invites (7-day expiry). |
| **Role management** | `owner`, `parentAdmin`, `organizer`, `contributor`, `viewer`, `recipient` — the exact role values the app's `CollaborationRole` uses. |
| **Member directory** | List/add/update/remove members; removal cleans branch/folder scope lists. |
| **Family structure** | Branches and folders with per-member access lists. |
| **Device snapshots** | `/sync` stores raw platform database files as backup artifacts (not logical sync). |

## Quick start

The repository ships **no usable API credentials** — create a token first:

```bash
cp api_keys.txt api_keys.txt.local
printf 'iphone:%s\n' "$(openssl rand -hex 32)" > api_keys.txt
docker compose pull
docker compose up -d
```

The server image is pulled from GHCR
(`ghcr.io/zippyy/letterstomy-selfhostedsync`) — no local Go compiler or
image build is needed.

Server listens on port 8080. In the app: **Settings → Self-Hosted Server**,
enter the server URL and the API token from `api_keys.txt`, enable, and tap
**Test Connection**.

## Architecture

- **Go** REST API, single binary (~9 MB), no external dependencies.
- **Bearer token auth** via `api_keys.txt` (`name:token` per line).
- **Persistent storage** on the `data` Docker volume (`/data`):
  - `collaboration.json` — members, invitations, branches, folders
  - `backup/*.letterstomy` — encrypted archives
  - `attachments/*` — media blobs
  - `sync/*-letters.db` — raw platform database snapshots
- **No database** — filesystem storage; restart preserves everything.

## API contract (v1)

- **Identity:** `GET /status` returns `service` (`LettersToMy-SelfHostedSync`),
  `api_version` (`1`), `server_version`, and `capabilities`
  (`collaboration`, `backups`, `attachments`). The client refuses to treat a
  bare 200 from an arbitrary endpoint as compatibility.
- **Timestamps:** Unix epoch **milliseconds** (JSON numbers).
- **Collections:** always JSON arrays — never `null`.
- **IDs:** `[A-Za-z0-9._-]{1,128}`. UUID strings, hex invite codes, and
  hyphenated names pass; anything else (including path traversal) is
  rejected with `400 invalid_request`.
- **Errors:** structured body `{"error":{"code":"...","message":"..."}}`:
  - `401 unauthorized` — bad/missing token
  - `404 not_found` — missing resource (includes PUT/DELETE on missing IDs)
  - `409 conflict` — duplicate member/branch/folder ID
  - `410 expired` — invitation expired (lookup or accept)
  - `413 payload_too_large` — upload exceeds the limit
    - `400 invalid_request` — e.g. folder in a nonexistent branch
  - `405 method_not_allowed`
  - `500 internal`
  - **Upload limits:** defaults are 1 MiB JSON bodies, 1 GiB sync
    snapshots, 256 MiB attachments, 1 GiB backups. Override with
    `MAX_JSON_BODY_SIZE`, `MAX_SYNC_SIZE`, `MAX_ATTACHMENT_SIZE`,
    `MAX_BACKUP_SIZE` (plain bytes or `K`/`M`/`G` suffixes, e.g. `1G`).
    `MAX_UPLOAD_BYTES` remains as a legacy alias that applies to every
    upload limit whose granular variable is not set.

## API reference

### Status

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/status` | Service identity, version, capabilities, counts, and listings |

### Backup — encrypted archive storage

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/backup/push?id=name` | Upload an archive (`id` optional; server generates one). Optional `letter_count` query param reports the number of letters inside the encrypted archive (the server cannot decrypt it; the client knows the count from its manifest) |
| `GET` | `/backup/pull/name` | Download an archive |
| `GET` | `/backup/list` | List stored archives (id, timestamp ms, size) |
| `DELETE` | `/backup/name` | Delete an archive |

### Attachments

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/attachment/upload?id=xyz` | Upload a blob |
| `GET` | `/attachment/list` | List stored blobs |
| `GET` | `/attachment/download/xyz` | Download a blob |
| `DELETE` | `/attachment/xyz` | Delete a blob |

### Collaboration — invitations and members

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/invite` | Create invitation (body: `created_by`, `role`, `branch_ids`, `folder_ids`; returns 6-char code, expires in 7 days) |
| `GET` | `/invite/:code` | Look up invitation (`410` when expired) |
| `POST` | `/invite/:code` | Accept (`member_id`, `member_name`; creates the member with the invited role + scope) |
| `DELETE` | `/invite/:code` | Revoke |
| `GET` | `/members` | List members |
| `PUT` | `/members` | Add/update member (id, name, role) |
| `DELETE` | `/members?id=x` | Remove member (also cleans branch/folder scope lists) |

Unknown roles are **rejected** with `400` — never silently remapped. An
invite created without a role defaults to `viewer` (least privilege).

### Family structure — branches and folders

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/branches` | List branches |
| `POST` | `/branches` | Create (id, name, kind; `409` on duplicate id) |
| `GET` | `/branches/:id` | Get one |
| `PUT` | `/branches/:id` | Update (404 when missing — no false success) |
| `DELETE` | `/branches/:id` | Delete (cascades to its folders) |
| `GET` | `/folders?branch_id=X` | List folders, optionally filtered |
| `POST` | `/folders` | Create (id, name, branch_id; 422 when branch missing) |
| `GET` | `/folders/:id` | Get one |
| `PUT` | `/folders/:id` | Update (404 when missing) |
| `DELETE` | `/folders/:id` | Delete |

Branch kinds: `parents`, `maternal`, `paternal`, `chosenFamily`, `custom`.

### Device snapshots (raw platform databases)

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/sync/push/ios` | Store the iOS database snapshot |
| `PUT` | `/sync/push/android` | Store the Android database snapshot |
| `GET` | `/sync/pull/ios` | Retrieve a snapshot |
| `GET` | `/sync/pull/android` | Retrieve a snapshot |
| `GET` | `/sync/list` | List snapshots (platform, timestamp ms, size, kind `device-snapshot`) |

These are **backup artifacts only**. The client does not expose them as live
sync and never hot-swaps a running Core Data store.

## Authentication

Edit `api_keys.txt` — one `name:token` per line:

```
iphone:a1b2c3d4...
android:e5f6g7h8...
web:i9j0k1l2...
```

Generate tokens:

```bash
openssl rand -hex 32
```

Clients send the token as `Authorization: Bearer <token>`. The file is
mounted read-only into the container at `/etc/letters2my/api_keys.txt`
(configurable with `API_KEYS_FILE`).

**The server refuses to start without a valid key file.** The shipped
`api_keys.txt` is a template only — it contains no usable credentials.
For local development only, you can allow the well-known development
credential `letters2my` by explicitly setting `ALLOW_INSECURE_DEFAULTS=true`
(this is off by default and never implied). In production, always generate
strong tokens (`openssl rand -hex 32`).

## Production notes

- Put it behind nginx or Caddy with TLS. The client validates the server
  identity over whatever transport you expose; HTTPS is required for
  anything beyond LAN testing (Apple ATS allows local networking by
  exception, not for arbitrary hosts).
- Local LAN testing with `http://192.168.x.x:8080` works because the app's
  Info.plist enables `NSAllowsLocalNetworking` — production traffic stays
  HTTPS-only.
- Back up the `data` volume — it contains every stored object.
- Raise `MAX_UPLOAD_BYTES` if you host large media.

## Health and status

| Endpoint | Auth | Purpose |
|----------|------|---------|
| `GET /healthz` | none | Liveness probe for container orchestrators; returns `204` when the process is up. Used by the Docker healthcheck. |
| `GET /status` | Bearer | Service identity, `api_version`, `server_version`, capabilities, counts, and listings. The client's Test Connection validates this response. |

## What this server currently does — and does not

Self-hosted storage currently provides:

- **Encrypted remote backup storage** — opaque `.letterstomy` archives
- **Remote backup listing, restore, and deletion**
- **Attachment API/storage** — upload, list, download, delete
- **Snapshot storage API** (`/sync`) — raw platform database files
- **Collaboration-directory API** — invitations, members, branches, folders

Current product boundaries (do not assume more):

- This is **not** a replacement for CloudKit logical record synchronization.
  CloudKit remains the source of truth for the LettersToMy archive; the
  server is an add-on for backup and directory state.
- Snapshot storage is **not** logical cross-platform letter synchronization:
  a Core Data database cannot be swapped into an Android app or vice versa,
  and the client never hot-swaps a live store.
- The collaboration endpoints expose a **directory and role API**, not a
  complete collaboration UI. There is no chat, no shared editing, and no
  conflict resolution.
- The server is **single-tenant / shared-state by design**. Every client
  that holds a valid API key shares one collaboration directory and one
  backup pool. There is no per-user isolation, multi-tenancy, or quota
  separation.

## Upgrading from an earlier build

The upgrade path is deliberately simple, and old data was explicitly tested
against this release:

1. Stop the existing container/server:
   ```bash
   docker compose down
   ```
2. Back up the data volume (see Disaster recovery below). This is your
   safety net; upgrades are tested to preserve state, but backups are cheap.
3. Update the image and restart:
   ```bash
   docker compose pull       # image installs
   # git pull                # source installs only
   docker compose up -d
   ```
4. On startup the server automatically reads supported existing state:
   - `collaboration.json` is read, validated, and migrated to the current
     schema version in place (legacy role values such as `editor` are
     explicitly remapped to the current role set).
   - Backup archives need no migration: archives are opaque encrypted
     blobs and are read straight from the `backup/` directory.
   - Legacy backups that predate the `letter_count` metadata sidecar remain
     fully readable and simply report a fallback letter count of `0` in
     listings.

Downgrade compatibility is not guaranteed: after the server has rewritten
`collaboration.json` to the current schema version, an older binary may not
understand it. Upgrades are one-way; keep a full data backup if you might
need to revert.

## Disaster recovery

The data directory (the Docker `data` volume, mounted at `/data`) is the
**entire state of the server**. It contains:

- `collaboration.json` — members, invitations, branches, folders
- `backup/*.letterstomy` — the encrypted backup archives (the valuable part)
- `backup/*.letterstomy.meta` — backup metadata sidecars (letter counts)
- `attachments/*` — uploaded media blobs
- `sync/*` — raw platform database snapshots
- `api_keys.txt` — **not** in the volume (mounted from the host)

**Back up the whole data directory.** Do not cherry-pick individual files:
a backup archive without its sidecar still works (it just loses the letter
count), but an archive without its directory structure, or a
`collaboration.json` without the archives, is only a partial restore. The
simplest reliable approach:

```bash
# With the container stopped (or a filesystem snapshot of the volume):
docker run --rm -v letterstomy-sync-data:/data -v "$PWD":/backup \
  alpine tar czf /backup/selfhosted-data-$(date +%F).tar.gz -C /data .
```

Restoring means replacing the whole volume contents from that archive, then
starting the server. Also preserve:

- `api_keys.txt` (or your equivalent key file) — without it the server
  refuses to start; with it, anyone holding the file has full access.
- Any reverse-proxy configuration that fronts the server.

## Security deployment checklist

- **Create a unique, strong API token** — `openssl rand -hex 32` per
  client, and give each device its own line in the key file so keys can be
  rotated individually.
- **Do not use `ALLOW_INSECURE_DEFAULTS` in production.** It is a
  development-only escape hatch (well-known credential `letters2my`); it is
  off by default and should stay off.
- **Terminate TLS using a trusted reverse proxy** (nginx, Caddy, Traefik)
  in front of the server. Do not publish plaintext HTTP directly to the
  public Internet.
- **Back up the persistent data volume** on a schedule (see Disaster
  recovery).
- **Protect the API key file** — it is mounted read-only into the
  container; on the host keep it readable only by the service account
  (`chmod 600`).
- **Restrict host/firewall exposure** — bind to the private interface or
  LAN, and/or allowlist client addresses; the server has no rate limiting,
  so exposure should be minimal.

## Docker

Prebuilt multi-architecture images (`linux/amd64`, `linux/arm64`) are
published to GitHub Container Registry on every push to `main` and on
every `v*` release tag:

```bash
docker pull ghcr.io/zippyy/letterstomy-selfhostedsync:latest
```

### Docker Compose

```bash
# first deployment, after configuring api_keys.txt (see Quick start)
docker compose pull
docker compose up -d
```

Server listens on port 8080. State lives in the named `data` volume
(mounted at `/data`) and survives container restarts.

### Upgrade

```bash
docker compose pull
docker compose up -d
```

Your `/data` volume is preserved — upgrades never touch it. Back it up
first (see Disaster recovery).

### Tags

| Tag | Meaning |
|-----|---------|
| `latest` | Most recent published build (main or latest stable release) |
| `main` | Latest build of the `main` branch |
| `sha-<shortsha>` | Image for a specific commit |
| `x.y.z` / `x.y` / `x` | Semver tags for `v*` releases |

For manual testing without Compose, use `docker run` directly
(`-v ./api_keys.txt:/etc/letters2my/api_keys.txt:ro` to mount your key
file).

## Testing

```bash
go test ./...
go vet ./...
go build ./...
```

### Cross-repo integration test

```bash
# From the server repo, with the client repo checked out next to it:
bash scripts/integration-test.sh
# or point at a client checkout:
LTM_CLIENT_REPO=/path/to/LettersToMy bash scripts/integration-test.sh
```

The harness builds the real Go server, starts it on a temp port with a temp
data dir, builds the Swift `selfhosted-check` executable from the client's
core package, and runs the **actual Swift client code against the actual
server over real HTTP**: identity/capability validation, collaboration round
trip, backup push→pull byte comparison, attachment round trip, wrong-token
401, and restart persistence. This is the executable proof that the two
implementations agree and cannot silently drift.