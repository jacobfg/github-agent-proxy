# Task runner for github-agent-proxy. Install just: https://just.systems

default: verify

build:
    cargo build --locked

build-release:
    cargo build --locked --release

fmt:
    cargo fmt --check

lint:
    cargo clippy --all-targets --locked -- -D warnings

test:
    cargo nextest run --locked

# All local gates, same as CI
verify: fmt lint test build

# Build the container image (docker; for Apple container use `container build -t github-agent-proxy .`)
docker-build:
    docker buildx build --load -t github-agent-proxy .

# Smoke-test a running proxy (default https://127.0.0.1:8443, self-signed ok)
smoke proxy="https://127.0.0.1:8443":
    ./scripts/smoke.sh {{proxy}}

# Regenerate THIRD_PARTY_NOTICES.md
notices:
    cargo about generate about.hbs > THIRD_PARTY_NOTICES.md
