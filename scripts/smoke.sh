#!/usr/bin/env bash
# Smoke test against a running proxy. The policy checks here need no working
# upstream: org allowlist and push ref-prefix rejections are answered locally.
#
# Usage: smoke.sh [proxy-base-url]   (default https://127.0.0.1:8443)
# The proxy must run with GAP_ALLOWED_ORGS=myorg for these checks.
set -euo pipefail

PROXY="${1:-https://127.0.0.1:8443}"

fail() { echo "smoke: FAIL: $*" >&2; exit 1; }

# 1. healthz
code=$(curl -sS -k -o /dev/null -w "%{http_code}" "$PROXY/healthz") || fail "healthz unreachable"
[ "$code" = 200 ] || fail "healthz returned $code"

# 2. write to a non-allowed org -> 403 policy_denied
body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
code=$(curl -sS -k -o "$body_file" -w "%{http_code}" -X POST \
  "$PROXY/api/v3/repos/notallowed-org/r/issues" \
  -H 'content-type: application/json' -d '{"title":"x"}')
[ "$code" = 403 ] || fail "expected 403 for non-allowed org, got $code"
grep -q policy_denied "$body_file" || fail "403 body missing policy_denied: $(cat "$body_file")"

# 3. default deny for unlisted writes (releases) even in an allowed org
code=$(curl -sS -k -o /dev/null -w "%{http_code}" -X POST \
  "$PROXY/api/v3/repos/myorg/r/releases" \
  -H 'content-type: application/json' -d '{"tag_name":"v0.0.1"}')
[ "$code" = 403 ] || fail "expected 403 for release create, got $code"

# 4. push ref-prefix rejection comes back as a receive-pack result with ng
#    lines (HTTP 200), which git renders as [remote rejected] ... (policy: ...)
Z=0000000000000000000000000000000000000000
A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
cmd="$Z $A refs/heads/main"
frame=$(printf '%04x%s' $(( ${#cmd} + 4 )) "$cmd")
out=$(curl -sS -k -X POST "$PROXY/myorg/r.git/git-receive-pack" \
  -H 'content-type: application/x-git-receive-pack-request' \
  --data-binary "${frame}0000")
echo "$out" | grep -q "ng refs/heads/main" || fail "receive-pack rejection missing ng line: $out"
echo "$out" | grep -q "policy:" || fail "rejection missing policy reason: $out"

# 5. prefixed branch push is NOT policy-rejected locally (upstream is
#    unreachable in smoke, so we just require the response isn't our
#    synthetic policy rejection)
cmd2="$Z $A refs/heads/agent/smoke"
frame2=$(printf '%04x%s' $(( ${#cmd2} + 4 )) "$cmd2")
out=$(curl -sS -k -X POST "$PROXY/myorg/r.git/git-receive-pack" \
  -H 'content-type: application/x-git-receive-pack-request' \
  --data-binary "${frame2}0000" || true)
case "$out" in
  *"policy:"*) fail "prefixed branch was policy-rejected: $out" ;;
esac

# 6. read passthrough to the real upstream (opt-in; needs network)
if [ "${GAP_SMOKE_READ:-0}" = 1 ]; then
  code=$(curl -sS -k -o /dev/null -w "%{http_code}" "$PROXY/api/v3/rate_limit")
  case "$code" in
    200|401) ;; # 401 = upstream answered (dummy token); both prove passthrough
    *) fail "read passthrough returned $code" ;;
  esac
fi

echo "smoke: OK"
