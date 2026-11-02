# Local development only. GitHub Actions runs its commands directly.
set dotenv-load
set positional-arguments
set shell := ["bash", "-euo", "pipefail", "-c"]

export PROXY_PRIVATE := env("PROXY_PRIVATE", env("HOME") + "/.config/github-agent-proxy-private")
export PROXY_IMAGE := env("PROXY_IMAGE", "github-agent-proxy:local")
export TEST_IMAGE := env("TEST_IMAGE", "github-agent-proxy:test")
export PROXY_NAME := env("PROXY_NAME", "github-agent-proxy")
export PROXY_PORT := env("PROXY_PORT", "8443")
export ALLOWED_ORGS := env("ALLOWED_ORGS", "")
export ALLOWED_USERS := env("ALLOWED_USERS", "")
export BRANCH_PREFIX := env("BRANCH_PREFIX", "agent/")

# List local development commands.
default:
    @just --list

# Start the Apple Container service.
system-start:
    container system start

# Install the mkcert CA and generate local certificates. Restart the proxy afterwards.
certs:
    install -d -m 700 "$PROXY_PRIVATE"
    mkcert -install
    mkcert -cert-file "$PROXY_PRIVATE/server.pem" -key-file "$PROXY_PRIVATE/server-key.pem" localhost 127.0.0.1 ::1 api.github.com github.com
    cp "$(mkcert -CAROOT)/rootCA.pem" "$PROXY_PRIVATE/rootCA.pem"
    touch "$PROXY_PRIVATE/token"
    chmod 600 "$PROXY_PRIVATE/token" "$PROXY_PRIVATE/server-key.pem"

# Build the production proxy image with Apple Container.
build:
    container build -t "$PROXY_IMAGE" .

# Build and start the proxy, publishing only on host loopback.
run: _check-private build
    container run --detach --name "$PROXY_NAME" \
      --publish "127.0.0.1:$PROXY_PORT:8443" \
      --user "$(id -u):$(id -g)" --read-only \
      --mount "type=bind,source=$PROXY_PRIVATE,target=/run/proxy,readonly" \
      --env GITHUB_TOKEN_FILE=/run/proxy/token \
      --env ALLOWED_ORGS --env ALLOWED_USERS --env BRANCH_PREFIX \
      --env LISTEN_ADDR=0.0.0.0:8443 --env ALLOW_CONTAINER_BIND=1 \
      --env TLS_CERT_FILE=/run/proxy/server.pem --env TLS_KEY_FILE=/run/proxy/server-key.pem \
      "$PROXY_IMAGE"

# Follow the running proxy's logs.
logs:
    container logs --follow "$PROXY_NAME"

# Stop the named proxy container.
stop:
    container stop "$PROXY_NAME"

# Delete the stopped proxy container so it can be recreated with new settings.
remove:
    container delete "$PROXY_NAME"

# Build the image containing Go, the proxy, gh, Git, and the test sources.
test-build:
    container build -f Dockerfile.test -t "$TEST_IMAGE" .

# Build and run the full offline race-test suite; no credentials are mounted.
test: test-build
    container run --rm "$TEST_IMAGE"

# Open a disposable test shell; no credentials are mounted.
test-shell: test-build
    container run --rm -it "$TEST_IMAGE" bash

# Report ALLOW/BLOCK for the configured policy and simulated OWNER/REPO (User or Organization).
policy repo owner_type="User": _check-policy test-build
    container run --rm \
      --env POLICY_CHECK=1 --env "POLICY_CHECK_REPO=$1" --env "POLICY_CHECK_OWNER_TYPE=$2" \
      --env ALLOWED_ORGS --env ALLOWED_USERS --env BRANCH_PREFIX \
      "$TEST_IMAGE" go test -v -count=1 ./internal/proxy -run '^TestConfiguredPolicy$'

# Start the proxy and an interactive gh/git shell together, using the mounted test PAT.
test-live: _check-private test-build
    #!/usr/bin/env bash
    set -euo pipefail
    test -s "$PROXY_PRIVATE/rootCA.pem" || { printf 'Missing public root CA; run just certs.\n' >&2; exit 1; }
    container run --rm -it \
      --mount "type=bind,source=$PROXY_PRIVATE,target=/run/proxy,readonly" \
      --env GITHUB_TOKEN_FILE=/run/proxy/token \
      --env TLS_CERT_FILE=/run/proxy/server.pem --env TLS_KEY_FILE=/run/proxy/server-key.pem \
      --env ALLOWED_ORGS --env ALLOWED_USERS --env BRANCH_PREFIX \
      "$TEST_IMAGE" bash -euc '
        openssl verify -CAfile /run/proxy/rootCA.pem /run/proxy/server.pem
        github-agent-proxy >/tmp/github-agent-proxy.log 2>&1 &
        proxy_pid=$!
        trap "kill $proxy_pid 2>/dev/null || true" EXIT

        export GH_TOKEN=proxy-placeholder GH_PROMPT_DISABLED=1
        export HTTPS_PROXY=https://127.0.0.1:8443
        export https_proxy="$HTTPS_PROXY"
        export NO_PROXY=localhost,127.0.0.1,::1,.githubusercontent.com,codeload.github.com
        export no_proxy="$NO_PROXY"
        export SSL_CERT_FILE=/run/proxy/rootCA.pem
        export GIT_SSL_CAINFO="$SSL_CERT_FILE"
        git config --global http.proxySSLCAInfo "$SSL_CERT_FILE"
        git config --global http."https://github.com/".proxy "$HTTPS_PROXY"
        git config --global --add url."https://github.com/".insteadOf git@github.com:
        git config --global --add url."https://github.com/".insteadOf ssh://git@github.com/
        git config --global credential."https://github.com".helper ""

        ready=0
        for attempt in $(seq 1 40); do
          if curl --noproxy "*" --cacert "$SSL_CERT_FILE" --max-time 2 \
              --fail --silent --output /dev/null https://127.0.0.1:8443/api/user; then
            ready=1
            break
          fi
          if ! kill -0 "$proxy_pid" 2>/dev/null; then break; fi
          sleep 1
        done
        if [ "$ready" != 1 ]; then cat /tmp/github-agent-proxy.log; exit 1; fi
        printf "Proxy ready. Use normal gh/git commands. Logs: /tmp/github-agent-proxy.log\n"
        bash --noprofile --norc -i
      '

[private]
_check-policy:
    @test -n "$ALLOWED_ORGS$ALLOWED_USERS" || { printf 'Set ALLOWED_ORGS and/or ALLOWED_USERS in .env or your environment.\n' >&2; exit 1; }

[private]
_check-private: _check-policy
    @for name in token server.pem server-key.pem; do test -s "$PROXY_PRIVATE/$name" || { printf 'Missing or empty file: %s/%s (run just certs, then put your PAT in token).\n' "$PROXY_PRIVATE" "$name" >&2; exit 1; }; done
