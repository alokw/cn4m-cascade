#!/usr/bin/env bash
# Walks the Phase 1 exit criteria (SPEC.md §11) with curl and prints every
# response, so a human can judge whether the error messages are legible.
#
# Runs inside the dev container: `make demo`.
set -uo pipefail

BASE="http://127.0.0.1:2649"
DATA_DIR="$(mktemp -d)"
MOUNT_ROOT="/mnt/smb-demo"
SAMBA_A="${SMBSYNC_TEST_SAMBA_A:-172.28.0.10}"
OFFLINE="${SMBSYNC_TEST_OFFLINE:-172.28.0.99}"

pass=0
fail=0

cleanup() {
    [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null
    wait "${SERVER_PID:-}" 2>/dev/null
    rm -rf "$DATA_DIR"
}
trap cleanup EXIT

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
good() { printf '\033[32m  PASS\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad()  { printf '\033[31m  FAIL\033[0m %s\n' "$*"; fail=$((fail + 1)); }

# create_target NAME SHARE USER PASS HOST -> prints the new target's id
create_target() {
    curl -sS -X POST "$BASE/api/targets" \
        -H 'Content-Type: application/json' \
        -d "{\"name\":\"$1\",\"type\":\"smb\",\"host\":\"$5\",\"share\":\"$2\",\"username\":\"$3\",\"password\":\"$4\"}" \
        | jq -r '.id // empty'
}

test_target() { curl -sS -o /tmp/body.json -w '%{http_code}' -X POST "$BASE/api/targets/$1/test"; }

show() { jq . /tmp/body.json; }

mounts_under() { grep -c " $MOUNT_ROOT/" /proc/self/mountinfo 2>/dev/null; true; }

# ---------------------------------------------------------------------------

mkdir -p "$MOUNT_ROOT"

say "Starting the server"
ENCRYPTION_KEY="demo-encryption-key-not-for-production" \
DATA_DIR="$DATA_DIR" \
MOUNT_ROOT="$MOUNT_ROOT" \
MOUNT_IDLE_GRACE="5s" \
MOUNT_TIMEOUT="15s" \
LISTEN_ADDR=":2649" \
    /tmp/smbsync &
SERVER_PID=$!

for _ in $(seq 1 50); do
    curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
    sleep 0.2
done
curl -sf "$BASE/healthz" >/dev/null || { echo "server never came up"; exit 1; }

# ---------------------------------------------------------------------------
say "1. Add an SMB target by IP"
ID_OK=$(create_target "nas-a" "private" "syncuser" "syncpass" "$SAMBA_A")
if [ -n "$ID_OK" ]; then good "created target $ID_OK"; else bad "could not create the target"; fi
curl -sS "$BASE/api/targets/$ID_OK" | jq .

say "2. Test it (mount + statfs + list root)"
code=$(test_target "$ID_OK"); show
if [ "$code" = "200" ]; then good "HTTP $code"; else bad "HTTP $code, want 200"; fi
jq -e '.negotiated_vers | length > 0' /tmp/body.json >/dev/null \
    && good "recorded the negotiated SMB version" \
    || bad "no negotiated SMB version recorded"
jq -e '.entries | map(.name) | index("hello.txt")' /tmp/body.json >/dev/null \
    && good "listed the fixture files" \
    || bad "root listing is missing the fixtures"

say "3. The mount survives the test itself (idle grace)"
held=$(mounts_under)
if [ "$held" -ge 1 ]; then good "$held mount(s) held for reuse"; else bad "the mount was dropped immediately"; fi

say "3b. Guest share (no credentials)"
ID_GUEST=$(curl -sS -X POST "$BASE/api/targets" -H 'Content-Type: application/json' \
    -d "{\"name\":\"guest\",\"type\":\"smb\",\"host\":\"$SAMBA_A\",\"share\":\"public\"}" | jq -r '.id')
code=$(test_target "$ID_GUEST"); show
if [ "$code" = "200" ]; then good "HTTP $code"; else bad "HTTP $code, want 200"; fi

# ---- the legibility cases: read these messages and judge them -------------
say "4. Wrong password  <-- is this message legible?"
ID_BAD=$(create_target "bad-creds" "private" "syncuser" "wrong-password" "$SAMBA_A")
code=$(test_target "$ID_BAD"); show
if [ "$code" = "401" ]; then good "HTTP $code"; else bad "HTTP $code, want 401"; fi

say "5. Share does not exist  <-- is this message legible?"
ID_SHARE=$(create_target "bad-share" "no-such-share" "syncuser" "syncpass" "$SAMBA_A")
code=$(test_target "$ID_SHARE"); show
if [ "$code" = "502" ]; then good "HTTP $code"; else bad "HTTP $code, want 502"; fi

say "6. Offline host  <-- is this message legible, and is it fast?"
ID_OFF=$(create_target "offline" "media" "syncuser" "syncpass" "$OFFLINE")
started=$(date +%s)
code=$(test_target "$ID_OFF"); show
elapsed=$(( $(date +%s) - started ))
printf '  failed in %ss\n' "$elapsed"
if [ "$elapsed" -lt 30 ]; then good "bounded (${elapsed}s < 30s)"; else bad "took ${elapsed}s; nothing may hang"; fi

# ---------------------------------------------------------------------------
say "7. Mounts clean up"
printf '  mounts now: %s\n' "$(mounts_under)"
printf '  waiting out the 5s idle grace...\n'
sleep 12
left=$(mounts_under)
if [ "$left" = "0" ]; then good "no mounts left under $MOUNT_ROOT"; else bad "$left mount(s) still present"; fi
grep " $MOUNT_ROOT/" /proc/self/mountinfo 2>/dev/null || true

say "8. Credentials never hit the process table or the API"
curl -sS "$BASE/api/targets" | jq -e '[.targets[] | select(has("password") or has("password_encrypted"))] | length == 0' >/dev/null \
    && good "no passwords in API responses" \
    || bad "the API returned a password field"
if compgen -G "$DATA_DIR/creds/creds-*" >/dev/null; then
    bad "credentials files left on disk:"; ls -la "$DATA_DIR/creds"
else
    good "no leftover credentials files"
fi

# ---------------------------------------------------------------------------
printf '\n\033[1m%s passed, %s failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
