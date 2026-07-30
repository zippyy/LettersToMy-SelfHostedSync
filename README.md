# Letters to My — Self-Hosted Sync Server

Docker-based sync, backup, and storage server for cross-platform
[Letters to My](https://github.com/zippyy/LettersToMy) clients.

## Quick start

```bash
docker compose up -d
```

Server listens on port 8080. Set `API_TOKEN` in your client settings.

## Architecture

- Go REST API, single binary, ~8MB
- Bearer token auth (api_keys.txt)
- File storage on mounted volume (`/data`)
- No database — filesystem stores sync snapshots, attachments, backups

## API

### Sync (cross-platform database push/pull)

| Method | Path | Description |
|--------|------|-------------|
| PUT | `/sync/push/ios` | Push iOS database snapshot |
| PUT | `/sync/push/android` | Push Android database snapshot |
| GET | `/sync/pull/ios` | Pull latest iOS database |
| GET | `/sync/pull/android` | Pull latest Android database |
| GET | `/sync/list` | List all platform snapshots |

### Attachments

| Method | Path | Description |
|--------|------|-------------|
| PUT | `/attachment/upload?id=xyz` | Upload an attachment |
| GET | `/attachment/download/xyz` | Download an attachment |

### Backup

| Method | Path | Description |
|--------|------|-------------|
| PUT | `/backup/push?id=backup-1` | Upload a .letterstomy backup |
| GET | `/backup/pull/backup-1` | Download a backup |
| GET | `/backup/list` | List all backups |

### Status

| Method | Path | Description |
|--------|------|-------------|
| GET | `/status` | List syncs, attachments, backups |

## Authentication

Edit `api_keys.txt` — one `name:token` per line. Generate tokens:

```bash
openssl rand -hex 32
```

Set the token in the app's Settings → Self-Hosted → API Token.

## This server is optional

The iOS app uses iCloud (CloudKit) by default. Android uses Google Drive.
This self-hosted server is an optional addon for users who want:
- Cross-platform sync between iOS and Android
- Private, self-contained storage
- No cloud vendor dependency