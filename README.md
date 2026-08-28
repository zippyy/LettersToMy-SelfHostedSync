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
docker compose up -d --build
```

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

## Docker

```bash
docker compose up -d      # build + run on :8080
docker compose down       # stop (volume persists)
```

State lives in the named `data` volume and survives container restarts.

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