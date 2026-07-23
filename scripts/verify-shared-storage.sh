#!/usr/bin/env bash
# verify-shared-storage.sh — cross-node workspace visibility check for
# storage.workspace.backend == "shared".
#
# Proves that two blowball instances sharing one MinIO-backed JuiceFS mount see
# the same per-user workspace: a file uploaded through node A is immediately
# listable and readable on node B (close-to-open consistency), and a delete on A
# is observed as not-found on B. This exercises the same fs.Store paths the
# xizhi_* agent tools use, so it covers the cross-instance consistency
# requirement of the workspace-shared-storage spec.
#
# Requires: curl, jq. Both nodes must share the same jwt.secret and the same
# MinIO bucket + JuiceFS metadata engine.
#
# Usage:
#   scripts/verify-shared-storage.sh \
#     --a http://node-a:8080 --b http://node-b:8080 \
#     --user alice --pass 's3cret'
#
# Exit codes: 0 = all checks passed; 1 = at least one check failed.

set -euo pipefail

NODE_A=""
NODE_B=""
USERNAME=""
PASSWORD=""

usage() {
  sed -n '3,28p' "$0" >&2
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --a)    NODE_A="$2"; shift 2 ;;
    --b)    NODE_B="$2"; shift 2 ;;
    --user) USERNAME="$2"; shift 2 ;;
    --pass) PASSWORD="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown arg: $1" >&2; usage ;;
  esac
done

for v in NODE_A NODE_B USERNAME; do
  if [[ -z "${!v}" ]]; then echo "missing --${v,,}" >&2; usage; fi
done

command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v jq   >/dev/null || { echo "jq is required"   >&2; exit 2; }

fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=1; }
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

# Login on each node; tokens are signed with the shared jwt.secret so a token
# from A is also valid on B, but we log in on both to confirm each instance
# reaches the shared MySQL user store.
login() {
  local base="$1"
  curl -fsS "$base/api/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg u "$USERNAME" --arg p "$PASSWORD" '{username:$u,password:$p}')" \
    | jq -r '.access_token'
}

MARKER="shared-probe-$(date +%s)-$$"
PAYLOAD="cross-node-visibility-probe-${MARKER}"

step "Logging in on node A ($NODE_A) and node B ($NODE_B)"
TOKEN_A=$(login "$NODE_A") || { bad "login on A failed"; exit 1; }
TOKEN_B=$(login "$NODE_B") || { bad "login on B failed"; exit 1; }
ok "obtained tokens on both nodes"

step "1. Upload file '$MARKER.txt' via node A"
http_code=$(curl -s -o /dev/null -w '%{http_code}' \
  "$NODE_A/api/v1/workspace/upload" \
  -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@-;filename=$MARKER.txt" \
  -F "path=/" <<<"$PAYLOAD")
if [[ "$http_code" == "200" ]]; then ok "upload returned 200"; else bad "upload returned $http_code"; fi

step "2. List root workspace via node B — file should be visible immediately"
names=$(curl -fsS "$NODE_B/api/v1/workspace/files?path=/&include_hidden=1" \
  -H "Authorization: Bearer $TOKEN_B" | jq -r '.files[].name')
if grep -qx "$MARKER.txt" <<<"$names"; then ok "node B sees the file"; else bad "node B does NOT see the file (list: $(echo "$names" | tr '\n' ' '))"; fi

step "3. Read file content via node B — should match what node A wrote"
got=$(curl -fsS "$NODE_B/api/v1/workspace/files/$MARKER.txt/content" \
  -H "Authorization: Bearer $TOKEN_B")
if [[ "$got" == "$PAYLOAD" ]]; then ok "content matches"; else bad "content mismatch: got='$got' want='$PAYLOAD'"; fi

step "4. Delete file via node A"
http_code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
  "$NODE_A/api/v1/workspace/files/$MARKER.txt" \
  -H "Authorization: Bearer $TOKEN_A")
if [[ "$http_code" == "200" ]]; then ok "delete returned 200"; else bad "delete returned $http_code"; fi

step "5. Read deleted file via node B — should be not-found (404)"
http_code=$(curl -s -o /dev/null -w '%{http_code}' \
  "$NODE_B/api/v1/workspace/files/$MARKER.txt/content" \
  -H "Authorization: Bearer $TOKEN_B")
if [[ "$http_code" == "404" ]]; then ok "node B observes the delete (404)"; else bad "node B still sees the deleted file ($http_code)"; fi

echo ""
if [[ "$fail" -eq 0 ]]; then
  printf '\033[32mAll cross-node shared-storage checks passed.\033[0m\n'
  exit 0
else
  printf '\033[31mOne or more checks FAILED.\033[0m\n' >&2
  exit 1
fi
