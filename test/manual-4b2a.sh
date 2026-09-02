#!/usr/bin/env bash
# Sets up the two runs that the Phase 4b-2a manual pass needs, against a server
# already running via `make run`.
#
#   bash test/manual-4b2a.sh preview <nas-target-id> <scratch-subpath>
#   bash test/manual-4b2a.sh prompt  <nas-target-id> <scratch-subpath>
#
# It creates a throwaway *local* source inside the dev container and points a
# job at a scratch folder on your target. It never touches anything else.
#
# SAFETY: these jobs run in mirror mode, which deletes whatever is at the
# destination and not at the source. The subpath you pass MUST be a scratch
# folder holding nothing you want to keep. The preview run parks and shows you
# the deletions before doing anything; the prompt run does not preview, so its
# destination subpath is written to for real.
set -uo pipefail

BASE="${SMBSYNC_BASE:-http://localhost:8384}"
COOKIES="$(mktemp)"
MODE="${1:-}"
DEST="${2:-}"
DEST_SUBPATH="${3:-smbsync-scratch}"
# RFC 5737 TEST-NET-1: reserved for documentation and guaranteed unroutable, so
# it is a dependable stand-in for "a server that is not answering".
BOGUS_HOST="192.0.2.1"

die() { echo "error: $*" >&2; exit 1; }

[ -n "$MODE" ] && [ -n "$DEST" ] || die \
"usage: bash test/manual-4b2a.sh <preview|prompt> <target> [scratch-subpath]

  target           the NAME of an existing target (as shown on the Targets
                   screen), or its id. NOT a //host/share path — the target
                   already knows its host and share.
  scratch-subpath  a folder INSIDE that target, relative to its root, holding
                   nothing you want to keep. e.g. 'smbsync-scratch'. Not a path
                   on your Mac, and not absolute. Defaults to smbsync-scratch."

case "$MODE" in preview|prompt) ;; *) die "mode must be 'preview' or 'prompt'" ;; esac

command -v python3 >/dev/null || die "python3 is required (macOS ships it)"

case "$DEST_SUBPATH" in
    /*) die "scratch-subpath must be relative to the target root, not an absolute path.
       You passed '$DEST_SUBPATH', which looks like a path on your Mac. The target
       already points at its share; this is a folder inside it, e.g. 'smbsync-scratch'." ;;
    *..*) die "scratch-subpath must not contain '..'" ;;
esac

case "$DEST" in
    //*|\\\\*) die "target must be a target NAME or id, not a //host/share path.
       You passed '$DEST'. Create the target first (Targets screen), then pass its name." ;;
esac

# python3 rather than jq: jq is not installed by default on macOS, and this
# script runs on the host so it cannot borrow the dev container's copy.
json_field() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }
json_str()   { python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"; }

printf 'Admin password: ' >&2
read -rs PASSWORD
echo >&2

api() { curl -sS -b "$COOKIES" -c "$COOKIES" -H 'content-type: application/json' "$@"; }

# The server explains itself well; a generic "could not create the job" throws
# that away at exactly the moment it is needed.
explain() {
    python3 -c '
import json, sys
try:
    body = json.load(sys.stdin)
except Exception:
    sys.exit(0)
err = body.get("error")
if err:
    print("  server said: " + err.get("message", ""), file=sys.stderr)
    if err.get("detail"):
        print("  detail: " + err["detail"], file=sys.stderr)
'
}

[ "$(api -X POST "$BASE/api/auth/login" -d "{\"password\":$(json_str "$PASSWORD")}" \
    | json_field authenticated)" = "True" ] || die "login failed (wrong password?)"

# Resolve a name to an id, so nobody has to dig an id out of the API — the
# Targets screen does not show them.
DEST_ID=$(api "$BASE/api/targets" | python3 -c '
import json, sys
want = sys.argv[1]
targets = json.load(sys.stdin).get("targets") or []
for t in targets:
    if t["id"] == want or t["name"] == want:
        print(t["id"]); break
else:
    print("NOT_FOUND:" + ", ".join(t["name"] for t in targets) if targets else "NOT_FOUND:")
' "$DEST")

case "$DEST_ID" in
    NOT_FOUND:*) die "no target named \"$DEST\". Existing targets: ${DEST_ID#NOT_FOUND:}" ;;
    "") die "could not list targets" ;;
esac
echo "    destination target: $DEST_ID ($DEST), subpath \"$DEST_SUBPATH\""

echo "==> seeding a local source inside the dev container"
docker compose -f docker-compose.test.yml exec -T dev sh -c '
  rm -rf /tmp/smbsync-manual-src
  mkdir -p /tmp/smbsync-manual-src/nested
  echo hello > /tmp/smbsync-manual-src/a.txt
  echo world > /tmp/smbsync-manual-src/nested/b.txt
  head -c 3000000 /dev/urandom > /tmp/smbsync-manual-src/big.bin
' || die "could not seed the source (is the dev container up?)"

SRC=$(api -X POST "$BASE/api/targets" \
  -d '{"name":"manual-src-'"$RANDOM"'","type":"local","local_path":"/tmp/smbsync-manual-src",
       "host":"","share":"","username":"","password":""}' | json_field id)
[ -n "$SRC" ] || die "could not create the local source target"
echo "    source target: $SRC"

if [ "$MODE" = preview ]; then
    JOB_BODY=$(api -X POST "$BASE/api/jobs" -d "$(python3 -c '
import json, sys, time
src, dst, sub = sys.argv[1:4]
print(json.dumps({"name": "manual-preview-%d" % time.time(), "source_target_id": src,
                  "mode": "mirror",
                  "destinations": [{"dest_target_id": dst, "dest_subpath": sub}]}))
' "$SRC" "$DEST_ID" "$DEST_SUBPATH")")
    JOB=$(printf '%s' "$JOB_BODY" | json_field id)
    [ -n "$JOB" ] || { printf '%s' "$JOB_BODY" | explain; die "could not create the job"; }

    RUN=$(api -X POST "$BASE/api/jobs/$JOB/run" -d '{"preview":true}' | json_field id)
    [ -n "$RUN" ] || die "could not start the preview"

    cat >&2 <<EOF

==> Preview parked. Open:  $BASE/runs/$RUN

    What to check:
      - "Will be deleted" names each extraneous file individually, not just a count.
        (Put a junk file in "$DEST_SUBPATH" beforehand, or there will be nothing to delete.)
      - The countdown says when it cancels itself.
      - Confirm executes exactly what was listed. Cancel changes nothing.
EOF
else
    BOGUS=$(api -X POST "$BASE/api/targets" \
      -d '{"name":"unreachable-'"$RANDOM"'","type":"smb","host":"'"$BOGUS_HOST"'","share":"nothing"}' \
      | json_field id)
    [ -n "$BOGUS" ] || die "could not create the unreachable target"
    echo "    unreachable target: $BOGUS ($BOGUS_HOST)"

    JOB_BODY=$(api -X POST "$BASE/api/jobs" -d "$(python3 -c '
import json, sys, time
src, dst, sub, bad = sys.argv[1:5]
print(json.dumps({"name": "manual-prompt-%d" % time.time(), "source_target_id": src,
                  "mode": "mirror", "unavailable_policy": "prompt",
                  "prompt_timeout_sec": 180, "prompt_fallback": "skip",
                  "destinations": [{"dest_target_id": dst, "dest_subpath": sub},
                                   {"dest_target_id": bad, "dest_subpath": ""}]}))
' "$SRC" "$DEST_ID" "$DEST_SUBPATH" "$BOGUS")")
    JOB=$(printf '%s' "$JOB_BODY" | json_field id)
    [ -n "$JOB" ] || { printf '%s' "$JOB_BODY" | explain; die "could not create the job"; }

    RUN=$(api -X POST "$BASE/api/jobs/$JOB/run" -d '{}' | json_field id)
    [ -n "$RUN" ] || die "could not start the run"

    cat >&2 <<EOF

==> Run started. Open:  $BASE/runs/$RUN

    What to check:
      - A modal appears for the unreachable destination, with a countdown to
        the fallback (skip, after 180s).
      - THE POINT: your real destination finishes copying WHILE that modal is
        still open. One stalled destination must not hold up a healthy one.
      - Skip -> the run ends "partial". Abort -> "failed".
EOF
fi

rm -f "$COOKIES"
