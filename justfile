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
export POLICY_FILE := env("POLICY_FILE", "")
export BRANCH_PREFIX := env("BRANCH_PREFIX", if POLICY_FILE == "" { "agent/" } else { "" })
export INTERCEPT_CA_CERT_FILE := env("INTERCEPT_CA_CERT_FILE", "")
export INTERCEPT_CA_KEY_FILE := env("INTERCEPT_CA_KEY_FILE", "")

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
    python3 scripts/container-run.py run

# Build a native proxy in the protected directory, outside the agent workspace.
host-build: _check-private
    install -d -m 700 "$PROXY_PRIVATE"
    go build -trimpath -o "$PROXY_PRIVATE/github-agent-proxy.new" ./cmd/github-agent-proxy
    chmod 700 "$PROXY_PRIVATE/github-agent-proxy.new"
    mv "$PROXY_PRIVATE/github-agent-proxy.new" "$PROXY_PRIVATE/github-agent-proxy"

# Start/restart on the Mac's network (VPN/private services); stop the container first.
host-start: host-build
    #!/usr/bin/env python3
    import os, pathlib, plistlib, socket, subprocess
    private = pathlib.Path(os.environ["PROXY_PRIVATE"]).resolve()
    label = "local.github-agent-proxy"
    domain = f"gui/{os.getuid()}"
    service = f"{domain}/{label}"
    if subprocess.run(["launchctl", "print", service], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
        subprocess.run(["launchctl", "bootout", service], check=True)
    port = int(os.environ["PROXY_PORT"])
    with socket.socket() as probe:
        try:
            probe.bind(("127.0.0.1", port))
        except OSError:
            raise SystemExit(f"Port {port} is in use. Stop the container with just stop before starting the host proxy.")
    environment = {key: os.environ[key] for key in ("ALLOWED_ORGS", "ALLOWED_USERS", "BRANCH_PREFIX", "POLICY_FILE", "INTERCEPT_CA_CERT_FILE", "INTERCEPT_CA_KEY_FILE", "SSL_CERT_FILE", "SSL_CERT_DIR") if os.environ.get(key)}
    environment.update(GITHUB_TOKEN_FILE=str(private / "token"), LISTEN_ADDR=f"127.0.0.1:{port}", TLS_CERT_FILE=str(private / "server.pem"), TLS_KEY_FILE=str(private / "server-key.pem"))
    if "MAX_PUSH_BYTES" in os.environ:
        environment["MAX_PUSH_BYTES"] = os.environ["MAX_PUSH_BYTES"]
    log = private / "host-proxy.log"
    log.touch(mode=0o600, exist_ok=True)
    log.chmod(0o600)
    config = {"Label": label, "ProgramArguments": [str(private / "github-agent-proxy")], "WorkingDirectory": str(private), "EnvironmentVariables": environment, "StandardOutPath": str(log), "StandardErrorPath": str(log), "RunAtLoad": True, "KeepAlive": {"SuccessfulExit": False}, "ThrottleInterval": 10}
    plist = private / "host-proxy.plist"
    plist.write_bytes(plistlib.dumps(config))
    plist.chmod(0o600)
    subprocess.run(["launchctl", "bootstrap", domain, str(plist)], check=True)
    print(f"Host proxy launched on https://127.0.0.1:{port}; check just host-logs for readiness.")

# Stop the native proxy before switching back to Apple Container.
host-stop:
    launchctl bootout "gui/$(id -u)/local.github-agent-proxy"

# Follow the native proxy's startup and runtime logs.
host-logs:
    tail -n 30 -f "$PROXY_PRIVATE/host-proxy.log"

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
    python3 scripts/container-run.py policy "$1" "$2"

# Start the proxy and interactive gh/git shell with the mounted test PAT.
test-live: _check-private test-build
    python3 scripts/container-run.py live

# Generate a dedicated interception CA; existing files are never overwritten.
intercept-ca:
    install -d -m 700 "$PROXY_PRIVATE"
    go run ./cmd/github-agent-proxy init-ca --cert "$PROXY_PRIVATE/intercept-ca.pem" --key "$PROXY_PRIVATE/intercept-ca-key.pem"

# Prepare/start an isolated native trial on 8444, preserving the current service.
trial-start:
    python3 scripts/trial.py start

trial-prepare:
    python3 scripts/trial.py prepare

trial-stop:
    python3 scripts/trial.py stop

trial-status:
    python3 scripts/trial.py status

# Add an opt-in launcher and profile to the companion Safehouse checkout.
trial-sandbox safehouse_repo:
    python3 scripts/install-safehouse-trial.py "$1"

[private]
_check-policy:
    @test -n "$POLICY_FILE$ALLOWED_ORGS$ALLOWED_USERS" || { printf 'Set ALLOWED_ORGS and/or ALLOWED_USERS in .env or your environment.\n' >&2; exit 1; }

[private]
_check-private: _check-policy
    @for name in token server.pem server-key.pem; do test -s "$PROXY_PRIVATE/$name" || { printf 'Missing or empty file: %s/%s (run just certs, then put your PAT in token).\n' "$PROXY_PRIVATE" "$name" >&2; exit 1; }; done
