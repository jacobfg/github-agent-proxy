#!/usr/bin/env bash
set -euo pipefail
openssl verify -CAfile /run/proxy/rootCA.pem /run/proxy/server.pem
github-agent-proxy >/tmp/github-agent-proxy.log 2>&1 &
proxy_pid=$!
trap "kill $proxy_pid 2>/dev/null || true" EXIT

export GH_TOKEN=proxy-placeholder GH_PROMPT_DISABLED=1
export HTTPS_PROXY=https://127.0.0.1:8443
export https_proxy="$HTTPS_PROXY"
export HTTP_PROXY="$HTTPS_PROXY" http_proxy="$HTTPS_PROXY"
export ALL_PROXY="$HTTPS_PROXY" all_proxy="$HTTPS_PROXY"
export NO_PROXY= no_proxy=
cat /etc/ssl/certs/ca-certificates.crt /run/proxy/rootCA.pem > /tmp/proxy-client-ca.pem
if [ -n "${INTERCEPT_CA_CERT_FILE:-}" ]; then cat "$INTERCEPT_CA_CERT_FILE" >> /tmp/proxy-client-ca.pem; fi
export SSL_CERT_FILE=/tmp/proxy-client-ca.pem
export GIT_SSL_CAINFO="$SSL_CERT_FILE"
printf "proxy-cacert = \"%s\"\n" "$SSL_CERT_FILE" > "$HOME/.curlrc"
git config --global http.proxySSLCAInfo "$SSL_CERT_FILE"
git config --global http."https://github.com/".proxy "$HTTPS_PROXY"
git config --global --add url."https://github.com/".insteadOf git@github.com:
git config --global --add url."https://github.com/".insteadOf ssh://git@github.com/
git config --global credential."https://github.com".helper ""

ready=0
for attempt in $(seq 1 40); do
  if curl --noproxy "*" --cacert "$SSL_CERT_FILE" --max-time 2 \
      --fail --silent --output /dev/null https://127.0.0.1:8443/healthz; then
    ready=1
    break
  fi
  if ! kill -0 "$proxy_pid" 2>/dev/null; then break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then cat /tmp/github-agent-proxy.log; exit 1; fi
printf "Proxy ready. Use normal gh/git commands. Logs: /tmp/github-agent-proxy.log\n"
bash --noprofile --norc -i
