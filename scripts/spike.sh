#!/usr/bin/env bash
# Live spike: verify the proxy against real GitHub, fully containerized.
#
# Two containers on a throwaway docker network: the proxy (holds the PAT) and
# an "agent" client (git/gh/curl, no real token). Nothing on the host is
# touched: no global git config, no env, no keychain, no mkcert needed (the
# test CA is generated with openssl in a helper container).
#
# Prereqs:
#   - a fine-grained PAT (Contents/Issues/Pull requests RW on the test repo),
#     provided via ONE of:
#       GAP_GITHUB_TOKEN=github_pat_...      (env var)
#       GAP_SPIKE_TOKEN_FILE=~/.secrets/gap-pat
#       ~/.secrets/gap-pat                   (default file location)
#   - proxy image built: docker buildx build --load -t gap-smoke .
#
# Usage:
#   GAP_SPIKE_REPO=owner/repo GAP_GITHUB_TOKEN=github_pat_... ./scripts/spike.sh
set -uo pipefail

REPO="${GAP_SPIKE_REPO:?set GAP_SPIKE_REPO=owner/repo (an empty test repo)}"
OWNER="${REPO%%/*}"
ORGS="${GAP_ALLOWED_ORGS:-$OWNER}"
IMAGE="${GAP_SPIKE_IMAGE:-gap-smoke}"
CLIENT_IMAGE="${GAP_SPIKE_CLIENT_IMAGE:-alpine:3}"
NET="gap-spike-net"
PROXY_NAME="gap-spike"

tmp=$(mktemp -d)
cleanup() {
  docker rm -f "$PROXY_NAME" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$tmp"
}
trap cleanup EXIT

# --- token ---------------------------------------------------------------
if [ -n "${GAP_GITHUB_TOKEN:-}" ]; then
  umask 077
  printf '%s' "$GAP_GITHUB_TOKEN" > "$tmp/token"
elif [ -n "${GAP_SPIKE_TOKEN_FILE:-}" ]; then
  cp "$GAP_SPIKE_TOKEN_FILE" "$tmp/token" 2>/dev/null \
    || { echo "cannot read GAP_SPIKE_TOKEN_FILE=$GAP_SPIKE_TOKEN_FILE"; exit 1; }
elif [ -f ~/.secrets/gap-pat ]; then
  cp ~/.secrets/gap-pat "$tmp/token"
else
  echo "no token: set GAP_GITHUB_TOKEN, GAP_SPIKE_TOKEN_FILE, or write ~/.secrets/gap-pat"
  exit 1
fi

# --- test CA + proxy cert (openssl in a helper container) -----------------
docker run --rm -v "$tmp:/certs" -w /certs "$CLIENT_IMAGE" sh -c '
  set -e
  apk add --no-cache -q openssl >/dev/null
  openssl req -x509 -newkey rsa:2048 -nodes -keyout ca-key.pem -out ca.pem \
    -days 2 -subj "/CN=gap-spike-test-ca" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes -keyout key.pem -out csr.pem \
    -subj "/CN=gap-spike" >/dev/null 2>&1
  printf "subjectAltName=DNS:gap-spike,DNS:localhost,IP:127.0.0.1\n" > san.ext
  openssl x509 -req -in csr.pem -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
    -out cert.pem -days 2 -extfile san.ext >/dev/null 2>&1
' || { echo "cert generation failed"; exit 1; }

# --- proxy container -------------------------------------------------------
docker network create "$NET" >/dev/null

docker run --name "$PROXY_NAME" -d --network "$NET" \
  -v "$tmp/token:/run/secrets/token:ro" \
  -v "$tmp/cert.pem:/certs/cert.pem:ro" \
  -v "$tmp/key.pem:/certs/key.pem:ro" \
  -e GAP_GITHUB_TOKEN_FILE=/run/secrets/token \
  -e GAP_TLS_CERT=/certs/cert.pem -e GAP_TLS_KEY=/certs/key.pem \
  -e GAP_ALLOWED_ORGS="$ORGS" \
  -e GAP_BRANCH_PREFIX=agent/ \
  "$IMAGE" >/dev/null || { echo "proxy failed to start"; exit 1; }

for i in $(seq 1 30); do
  docker exec "$PROXY_NAME" /github-agent-proxy healthcheck >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$PROXY_NAME" /github-agent-proxy healthcheck >/dev/null \
  || { echo "proxy never became healthy"; docker logs "$PROXY_NAME" | tail -5; exit 1; }

# --- agent container: the checks -------------------------------------------
# The agent environment: test CA mounted read-only, dummy token, interception
# config — exactly what a sandboxed coding agent would get.
docker run --rm -i --network "$NET" \
  -v "$tmp/ca.pem:/ca/rootCA.pem:ro" \
  -e GAP_SPIKE_REPO="$REPO" \
  "$CLIENT_IMAGE" sh -s << 'EOF'
set -uo pipefail
apk add --no-cache -q git github-cli curl >/dev/null || { echo "FAIL  apk install"; exit 1; }

export SSL_CERT_FILE=/ca/rootCA.pem
export GH_HOST=gap-spike:8443 GH_ENTERPRISE_TOKEN=dummy
export GIT_TERMINAL_PROMPT=0
git config --global url."https://gap-spike:8443/".insteadOf "https://github.com/"
git config --global http."https://gap-spike:8443/".sslCAInfo /ca/rootCA.pem

pass=0; fail=0
check() { # check <label> <rc> [output]
  if [ "$2" = 0 ]; then echo "PASS  $1"; pass=$((pass+1));
  else
    echo "FAIL  $1"; fail=$((fail+1))
    [ -n "${3:-}" ] && echo "$3" | sed 's/^/      /' | head -8
  fi
}

login=$(gh api user --jq .login 2>&1)
[ -n "$login" ] && [ "$login" != "null" ] && ! echo "$login" | grep -qi error
check "gh api user -> $login" $? "$login"

out=$(git clone "https://github.com/$GAP_SPIKE_REPO.git" /tmp/w 2>&1)
check "git clone" $? "$out"

cd /tmp/w 2>/dev/null || true
git -c user.email=spike@local -c user.name=spike commit -q --allow-empty -m spike 2>/dev/null
out=$(git push origin HEAD:refs/heads/agent/spike 2>&1)
check "git push agent/spike" $? "$out"

out=$(git push origin HEAD:refs/heads/main 2>&1)
rc=$?
{ [ $rc -ne 0 ] && echo "$out" | grep -q "remote rejected" && echo "$out" | grep -q "policy:"; }
check "push main remote-rejected with policy reason" $? "$out"

url=$(gh issue create --repo "$GAP_SPIKE_REPO" --title "proxy spike" --body "via github-agent-proxy" 2>&1)
case "$url" in http*) rc=0 ;; *) rc=1 ;; esac
check "gh issue create -> $url" $rc "$url"
# gh with a full URL ignores GH_HOST (talks to the URL's host) — use the
# number + --repo form, which is what agents should do through the proxy.
num="${url##*/}"
out=$(gh issue close "$num" --repo "$GAP_SPIKE_REPO" 2>&1)
check "gh issue close (owner-only)" $? "$out"

# explicit system bundle: SSL_CERT_FILE above points at the test CA, which
# (correctly) cannot verify api.github.com
code=$(curl --cacert /etc/ssl/cert.pem -sS -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer dummy" https://api.github.com/user 2>&1)
[ "$code" = 401 ]; check "bypass with dummy token fails closed (401)" $? "got: $code"

git push -q origin :refs/heads/agent/spike >/dev/null 2>&1  # cleanup

echo
echo "spike: $pass passed, $fail failed"
[ "$fail" = 0 ]
EOF
rc=$?
echo
if [ $rc -eq 0 ]; then echo "SPIKE OK"; else echo "SPIKE FAILED ($rc)"; fi
exit $rc
