# Letters to My — Self-Hosted Sync Server

Docker-based server providing **sync, backup, storage, and cross-platform
collaboration** for [Letters to My](https://github.com/zippyy/LettersToMy)
clients (iOS, Android, web).

## What it does

| Capability | Description |
|------------|-------------|
| **Cross-platform sync** | iOS ↔ Android database push/pull via `/sync` |
| **Backup storage** | Store `.letterstomy` archives on `/backup` |
| **Attachment hosting** | Upload/download photos, videos, audio on `/attachment` |
| **Invitations** | Create, look up, accept, revoke cross-platform invites |
| **Role management** | Owner, editor, contributor, viewer roles across devices |
| **Member directory** | List all members regardless of platform |

## Quick start

```bash
docker compose up -d
```

Server listens on port 8080. Set the server URL and API token
in the app under Settings → Self-Hosted.

## Architecture

- **Go** REST API, single binary (~9 MB)
- **Bearer token auth** via `api_keys.txt`
- **Persistent storage** on Docker volume (`/data`)
- **No database** — filesystem stores sync snapshots, attachments, backups, and collaboration state (`/data/collaboration.json`)

## API reference

### Sync — cross-platform database push/pull

Each platform pushes its Room/CoreData database to the server. Other
platforms pull the latest snapshot to stay in sync.

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/sync/push/ios` | Push iOS database |
| `PUT` | `/sync/push/android` | Push Android database |
| `GET` | `/sync/pull/ios` | Pull latest iOS database |
| `GET` | `/sync/pull/android` | Pull latest Android database |
| `GET` | `/sync/list` | List all platform snapshots with timestamps |

### Attachments

Store photos, videos, and audio files cross-platform.

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/attachment/upload?id=xyz` | Upload an attachment |
| `GET` | `/attachment/download/xyz` | Download an attachment |

### Backup — `.letterstomy` archive storage

Push and pull encrypted backup archives.

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/backup/push?id=backup-name` | Upload a backup |
| `GET` | `/backup/pull/backup-name` | Download a backup |
| `GET` | `/backup/list` | List all stored backups |

### Collaboration — invitations and roles

Cross-platform family collaboration. Works across iOS and Android —
one person creates an invite, the other accepts it on any platform.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/invite` | Create invitation (returns 6-char code, expires 7 days) |
| `GET` | `/invite/:code` | Look up invitation by code |
| `POST` | `/invite/:code` | Accept invitation (provide `member_id` and `member_name`) |
| `DELETE` | `/invite/:code` | Revoke invitation |
| `GET` | `/members` | List all members with roles |
| `PUT` | `/members` | Add or update a member's role |
| `DELETE` | `/members?id=xyz` | Remove a member |

**Roles**: `owner`, `editor`, `contributor`, `viewer`

### Family structure — branches and folders

Manage family sides (Parents, Maternal, Paternal, etc) and archive
folders. Access is scoped by member — each branch and folder lists
which member IDs can access it.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/branches` | List all family branches |
| `POST` | `/branches` | Create a new branch |
| `GET` | `/branches/:id` | Get a specific branch with members |
| `PUT` | `/branches/:id` | Update branch (rename, share with members) |
| `DELETE` | `/branches/:id` | Delete branch (cascades to folders) |
| `GET` | `/folders?branch_id=X` | List folders (optionally filtered by branch) |
| `POST` | `/folders` | Create a folder in a branch |
| `GET` | `/folders/:id` | Get a specific folder |
| `PUT` | `/folders/:id` | Update folder (rename, share with members) |
| `DELETE` | `/folders/:id` | Delete folder |

Branch kinds: `parents`, `maternal`, `paternal`, `chosenFamily`, `custom`

### Status

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/status` | Returns sync snapshots, attachments, and backups |

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

Clients send the token as `Authorization: Bearer <token>`.

## This server is optional

The iOS app uses iCloud (CloudKit) by default. Android uses Google Drive.
This self-hosted server is an addon for users who want:

- **Cross-platform sync** between iOS and Android (the main use case)
- **Cross-platform invitations** — invite family members regardless of device
- **Private, self-contained storage** — your data, your server
- **No cloud vendor dependency** — no Apple/Google account required

## Production notes

- Put it behind nginx or Caddy with TLS
- Use a reverse proxy for the `/backup` and `/sync` endpoints (larger payloads)
- Back up the `/data` volume — it contains all stored data