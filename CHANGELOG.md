# Changelog

All notable changes to LettersToMy-SelfHostedSync are recorded here.
This project follows the [SemVer](https://semver.org/) conventions already
in use by the server's `server_version` (`0.3.0`) and the shared API
contract (API v1).

## [0.3.0] - 2026-08-28

First tagged release. Companion server for LettersToMy (client release
v0.1.0 is the compatible pairing). Wire contract: **API v1**.

### Added

- **API v1 contract locked down** — service identity (`/status`),
  structured `{"error":{"code","message"}}` errors, full role set, safe
  identifier validation, `[]`-not-`null` collections, millisecond
  timestamps.
- **Secure API key startup** — the server refuses to start without a valid
  key file; the shipped `api_keys.txt` is a template with no usable
  credentials. `ALLOW_INSECURE_DEFAULTS` is off by default (development
  only).
- **Backup metadata** — `letter_count` persisted in sidecars, reported in
  listings and `/status`; legacy archives without sidecars report a
  fallback count of 0.
- **Attachment listing and deletion** (`/attachment/list`,
  `DELETE /attachment/:id`).
- **Backup deletion** (`DELETE /backup/:id`), including orphaned-sidecar
  cleanup.
- **Collaboration endpoints** — invitations (7-day expiry, role defaulting
  to `viewer`), members (with last-owner protection), branches, folders.
- **Cross-repository CI** — `.github/workflows/contract.yml` runs Go
  vet/test/build plus the real-HTTP integration harness against the Swift
  client's `selfhosted-check`.

### Changed

- **Atomic writes** for backups, attachments, and collaboration state
  (temp-file + rename); a failed metadata write no longer replaces a valid
  archive.
- **Concurrency-safe storage** — mutex-guarded state and per-store locks.
- **Improved error contract** — 400/401/404/409/410/413/500 with stable
  codes; PUT/DELETE on missing resources return 404 (no false success).
- **Last-owner protection** — the final owner can no longer be demoted or
  removed (409 `owner_required`).
- **Collaboration state validation and migration** — legacy role values
  (e.g. `editor`) are explicitly remapped and the schema version is
  persisted on upgrade.
- **Docker deployment fixes** — non-root container user, healthcheck on
  `/healthz`, Alpine runtime, read-only key-file mount.

### Compatibility

- API: **v1** (unchanged).
- Client: LettersToMy **v0.1.0** — same contract; the integration harness
  must stay green against the current client.
- Upgrade: existing `/data` state is read and migrated on startup; legacy
  backup archives remain readable (letter_count falls back to 0). Downgrade
  is not supported after migration.

### Notes

- Single-tenant / shared-state by design; not a CloudKit replacement and
  not logical cross-platform sync. See README "What this server currently
  does — and does not".