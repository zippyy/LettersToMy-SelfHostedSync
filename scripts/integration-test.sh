#!/usr/bin/env bash
# integration-test.sh — real-HTTP cross-repo integration test for
# LettersToMy (Swift client) × LettersToMy-SelfHostedSync (Go server).
#
# This is the executable proof that the two implementations agree on the
# wire contract. It is NOT a unit test: it builds and starts the actual
# Go server against a temporary data dir, then drives it with the actual
# Swift client code (the selfhosted-check executable built from the
# LettersToMyCore package) over real HTTP, then verifies:
#
#   1. server identity + capabilities contract (/status)
#   2. the full Swift capability probe (collaboration, backup, attachment
#      round trips with byte comparison) against the live server
#   3. backup upload → pull → byte-identical (sha256) round trip via curl
#   4. wrong token → 401 with a structured error body
#   5. server restart preserves both file-backed backups and
#      collaboration.json state
#
# Usage:
#   LTM_CLIENT_REPO=/path/to/LettersToMy bash scripts/integration-test.sh
#
# The client repo defaults to ../LettersToMy relative to the script.
# The active Xcode toolchain is used (DEVELOPER_DIR is honored) because
# the Swift checker must be buildable; on CI this is the macos runner's
# Xcode.
#
# Exit code 0 when every check passes; prints PASS/FAIL per step.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
CLIENT_REPO="${LTM_CLIENT_REPO:-$(cd "$SERVER_REPO/.." && pwd)/LettersToMy}"

if [[ ! -d "$CLIENT_REPO/Sources/LettersToMyCore" ]]; then
  echo "ERROR: client repo not found at $CLIENT_REPO (set LTM_CLIENT_REPO)" >&2
  exit 2
fi

TOKEN="integration-test-token-$(openssl rand -hex 8)"
PORT=$((20000 + RANDOM % 20000))
TMP_DIR="$(mktemp -d /tmp/ltm-integration.XXXXXX)"
SERVER_PID=""
FAILURES=0
PASSES=0

cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

API_KEYS_FILE="$TMP_DIR/api_keys.txt"
DATA_DIR="$TMP_DIR/data"
mkdir -p "$DATA_DIR"
printf 'integration:%s\n' "$TOKEN" > "$API_KEYS_FILE"

BASE="http://127.0.0.1:$PORT"
AUTH="Authorization: Bearer $TOKEN"

step() { printf '\n=== %s ===\n' "$*"; }
pass() { printf 'PASS  %s\n' "$*"; PASSES=$((PASSES+1)); }
fail() { printf 'FAIL  %s\n' "$*"; FAILURES=$((FAILURES+1)); }

health() { curl -sf -H "$AUTH" "$BASE/status" > /dev/null 2>&1; }

wait_health() {
  for _ in $(seq 1 60); do
    health && return 0
    sleep 0.5
  done
  return 1
}

start_server() {
  PORT="$PORT" DATA_DIR="$DATA_DIR" API_KEYS_FILE="$API_KEYS_FILE" \
    "$TMP_DIR/letters2my-sync" >> "$TMP_DIR/server.log" 2>&1 &
  SERVER_PID=$!
}

# ── 1. Build the server ──────────────────────────────────────────────
step "Build Go server"
( cd "$SERVER_REPO" && go build -o "$TMP_DIR/letters2my-sync" . )
pass "go build"

# ── 2. Start server, wait for health ─────────────────────────────────
step "Start server on port $PORT"
start_server
if wait_health; then
  pass "server healthy"
else
  fail "server never became healthy"
  tail -30 "$TMP_DIR/server.log" >&2 || true
  exit 1
fi

# ── 3. Status identity + non-null collection contract ────────────────
step "Status identity contract"
STATUS_JSON="$(curl -sf -H "$AUTH" "$BASE/status")"
echo "$STATUS_JSON" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["service"] == "LettersToMy-SelfHostedSync", d
assert d["api_version"] == 1, d
assert d["server_version"], d
assert {"collaboration", "backups", "attachments"} <= set(d["capabilities"]), d
for k in ("syncs", "attachments", "recoveries"):
    assert isinstance(d[k], list) and d[k] is not None, (k, d[k])
'
pass "service identity, api_version, capabilities, []-not-null"

# ── 4. Build the Swift contract checker ──────────────────────────────
step "Build Swift contract checker"
( cd "$CLIENT_REPO" && xcrun swift build -c release --product selfhosted-check )
pass "selfhosted-check built"

# ── 5. Swift capability probe against the LIVE server ────────────────
step "Swift client exercises live server (real HTTP)"
CHECKER="$CLIENT_REPO/.build/release/selfhosted-check"
if "$CHECKER" "$BASE" "$TOKEN"; then
  pass "selfhosted-check: identity + collaboration + backup + attachment all PASS"
else
  fail "selfhosted-check reported a failure"
fi

# ── 5b. Current-client backup E2E (real encrypted archives) ──────────
# Uses the CURRENT client's production BackupService (AES-256-GCM archive
# serialization) + SelfHostedAPIClient over real HTTP: letter_count
# semantics before/after deletion, byte-identical download, restore-decode
# of every collection, and backup deletion isolation. Only clients that
# ship the backup-e2e product run it (the released v0.1.0 client does
# not); older clients still get the full base contract above.
step "Current-client backup E2E (real archives)"
if grep -q 'backup-e2e' "$CLIENT_REPO/Package.swift"; then
  ( cd "$CLIENT_REPO" && xcrun swift build -c release --product backup-e2e )
  E2E="$CLIENT_REPO/.build/release/backup-e2e"
  if "$E2E" "$BASE" "$TOKEN"; then
    pass "backup-e2e: letter_count + deletion + restore + byte identity all PASS"
  else
    fail "backup-e2e reported a failure"
  fi
else
  printf 'SKIP  backup-e2e (client does not ship the product)\n'
fi

# ── 6. Backup byte round trip via curl ───────────────────────────────
step "Backup byte round trip"
PAYLOAD="$TMP_DIR/payload.bin"
PAYLOAD_DOWN="$TMP_DIR/payload.down.bin"
head -c 131072 /dev/urandom > "$PAYLOAD"

if curl -sf -X PUT -H "$AUTH" --data-binary @"$PAYLOAD" \
    "$BASE/backup/push?id=integration-backup" > /dev/null; then
  pass "backup upload"
else
  fail "backup upload"
fi

if curl -sf -H "$AUTH" "$BASE/backup/pull/integration-backup" -o "$PAYLOAD_DOWN" \
    && cmp -s "$PAYLOAD" "$PAYLOAD_DOWN"; then
  pass "backup pull byte-identical"
else
  fail "backup pull byte-identical"
fi

S1="$(shasum -a 256 "$PAYLOAD" | cut -d' ' -f1)"
S2="$(shasum -a 256 "$PAYLOAD_DOWN" | cut -d' ' -f1)"
if [[ "$S1" == "$S2" ]]; then
  pass "sha256 match ($S1)"
else
  fail "sha256 mismatch"
fi

# ── 7. Backup metadata contract ─────────────────────────────────────
step "Backup metadata contract (letter_count)"
META_BODY="$(curl -sf -X PUT -H "$AUTH" --data-binary @"$PAYLOAD" \
  "$BASE/backup/push?id=integration-meta&letter_count=42")"
if echo "$META_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for k in ("id", "timestamp", "size", "letter_count"):
    assert k in d, (k, d)
assert d["id"] == "integration-meta", d
assert d["letter_count"] == 42, d
assert d["timestamp"] > 0 and d["size"] > 0, d
'; then
  pass "backup push returns id/timestamp/size/letter_count"
else
  fail "backup push metadata (got: $META_BODY)"
fi

if curl -sf -H "$AUTH" "$BASE/backup/list" | python3 -c '
import json, sys
d = json.load(sys.stdin)
meta = [b for b in d if b["id"] == "integration-meta"]
assert meta and meta[0]["letter_count"] == 42, d
'; then
  pass "backup list reports persisted letter_count"
else
  fail "backup list letter_count"
fi

# ── 8. Wrong token → 401 + structured error ──────────────────────────
step "Authentication failure path"
CODE="$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer wrong-token" "$BASE/status")"
if [[ "$CODE" == "401" ]]; then
  pass "wrong token → 401"
else
  fail "wrong token → 401 (got $CODE)"
fi

ERR_BODY="$(curl -s -H "Authorization: Bearer wrong-token" "$BASE/status")"
if echo "$ERR_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["error"]["code"] == "unauthorized", d
assert d["error"]["message"], d
'; then
  pass "structured error body"
else
  fail "structured error body (got: $ERR_BODY)"
fi

# ── 9. Attachment lifecycle (list + delete) ──────────────────────────
step "Attachment list and delete"
if curl -sf -X PUT -H "$AUTH" --data-binary @"$PAYLOAD" \
    "$BASE/attachment/upload?id=integration-att.jpg" > /dev/null; then
  pass "attachment upload"
else
  fail "attachment upload"
fi

if curl -sf -H "$AUTH" "$BASE/attachment/list" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert any(a["id"] == "integration-att.jpg" for a in d), d
assert all("content_type" in a and "size" in a for a in d), d
'; then
  pass "attachment list with metadata"
else
  fail "attachment list with metadata"
fi

if curl -sf -H "$AUTH" "$BASE/attachment/download/integration-att.jpg" -o "$PAYLOAD_DOWN" \
    && cmp -s "$PAYLOAD" "$PAYLOAD_DOWN"; then
  pass "attachment download byte-identical"
else
  fail "attachment download byte-identical"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "$AUTH" \
  "$BASE/attachment/integration-att.jpg")"
if [[ "$CODE" == "204" || "$CODE" == "200" ]]; then
  pass "attachment delete"
else
  fail "attachment delete (got $CODE)"
fi

if curl -sf -H "$AUTH" "$BASE/attachment/list" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert not any(a["id"] == "integration-att.jpg" for a in d), d
'; then
  pass "attachment list reflects deletion"
else
  fail "attachment list reflects deletion"
fi

# ── 10. Backup delete ────────────────────────────────────────────────
step "Backup delete"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "$AUTH" \
  "$BASE/backup/integration-meta")"
if [[ "$CODE" == "204" || "$CODE" == "200" ]]; then
  pass "backup delete"
else
  fail "backup delete (got $CODE)"
fi

if curl -sf -H "$AUTH" "$BASE/backup/list" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert not any(b["id"] == "integration-meta" for b in d), d
'; then
  pass "backup list reflects deletion"
else
  fail "backup list reflects deletion"
fi

# ── 11. Create collaboration state that must survive a restart ───────
step "Seed collaboration state"
if curl -sf -X POST -H "$AUTH" -H "Content-Type: application/json" \
    -d '{"id":"restart-branch","name":"Persistence","kind":"custom"}' \
    "$BASE/branches" > /dev/null; then
  pass "branch created"
else
  fail "branch created"
fi

# ── 12. Restart server, verify persistence ───────────────────────────
step "Restart server"
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""
start_server
if wait_health; then
  pass "server healthy after restart"
else
  fail "server not healthy after restart"
  tail -30 "$TMP_DIR/server.log" >&2 || true
  exit 1
fi

step "State survives restart"
if curl -sf -H "$AUTH" "$BASE/backup/list" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert any(b["id"] == "integration-backup" for b in d), d
'; then
  pass "backup still listed after restart"
else
  fail "backup still listed after restart"
fi

if curl -sf -H "$AUTH" "$BASE/backup/pull/integration-backup" -o "$PAYLOAD_DOWN.2" \
    && cmp -s "$PAYLOAD" "$PAYLOAD_DOWN.2"; then
  pass "backup bytes identical after restart"
else
  fail "backup bytes identical after restart"
fi

if curl -sf -H "$AUTH" "$BASE/branches/restart-branch" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["id"] == "restart-branch" and d["name"] == "Persistence", d
'; then
  pass "collaboration state persisted across restart"
else
  fail "collaboration state persisted across restart"
fi

# ── 13. Contract regression locks ────────────────────────────────────
# Defects found in the adversarial review, fixed post-harness, now
# permanently locked at the real-HTTP level (the Go unit equivalents
# live in contract_regression_test.go):
#   a. POST /invite with an omitted role must default to viewer
#   b. demoting the FINAL owner must be rejected with 409 and leave
#      the owner untouched
step "Contract regression locks"

INVITE_BODY="$(curl -sf -X POST -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"created_by":"harness-inviter"}' "$BASE/invite")"
if echo "$INVITE_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d.get("role") == "viewer", d
assert d.get("code"), d
'; then
  pass "role-less invite defaults to viewer"
else
  fail "role-less invite defaults to viewer (got: $INVITE_BODY)"
fi

if curl -sf -X PUT -H "$AUTH" -H "Content-Type: application/json" \
    -d '{"id":"sole-owner","name":"Sole Owner","role":"owner"}' \
    "$BASE/members" > /dev/null; then
  pass "sole owner seeded"
else
  fail "sole owner seeded"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "$AUTH" \
  -H "Content-Type: application/json" \
  -d '{"id":"sole-owner","name":"Sole Owner","role":"viewer"}' "$BASE/members")"
if [[ "$CODE" == "409" ]]; then
  pass "final-owner demotion rejected with 409"
else
  fail "final-owner demotion rejected with 409 (got $CODE)"
fi

if curl -sf -H "$AUTH" "$BASE/members" | python3 -c '
import json, sys
d = json.load(sys.stdin)
m = [x for x in d if x["id"] == "sole-owner"]
assert m and m[0]["role"] == "owner", d
'; then
  pass "owner retained after rejected demotion"
else
  fail "owner retained after rejected demotion"
fi

# ── Summary ──────────────────────────────────────────────────────────
printf '\n=== INTEGRATION RESULT ===\n'
printf 'passed: %d   failed: %d\n' "$PASSES" "$FAILURES"
if [[ "$FAILURES" -eq 0 ]]; then
  printf 'INTEGRATION: ALL PASS\n'
  exit 0
fi
printf 'INTEGRATION: FAILED\n'
exit 1