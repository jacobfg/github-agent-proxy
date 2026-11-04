#!/usr/bin/env python3
"""Build safe argument arrays for Apple Container local development recipes."""
import os
from pathlib import Path
import subprocess
import sys


def main():
    action = sys.argv[1]
    private = Path(os.environ["PROXY_PRIVATE"]).resolve()
    args = ["container", "run"]
    if action == "run":
        args += ["--detach", "--name", os.environ["PROXY_NAME"], "--publish", f'127.0.0.1:{os.environ["PROXY_PORT"]}:8443',
                 "--user", f"{os.getuid()}:{os.getgid()}", "--read-only",
                 "--env", "LISTEN_ADDR=0.0.0.0:8443", "--env", "ALLOW_CONTAINER_BIND=1"]
    else:
        args += ["--rm"]
        if action == "live":
            args += ["-it"]
    policy = os.environ.get("POLICY_FILE")
    if policy:
        if any(os.environ.get(k) for k in ("ALLOWED_ORGS", "ALLOWED_USERS", "BRANCH_PREFIX")):
            raise SystemExit("POLICY_FILE conflicts with policy environment variables; remove them from .env/shell.")
    else:
        for key in ("ALLOWED_ORGS", "ALLOWED_USERS", "BRANCH_PREFIX"):
            args += ["--env", f"{key}={os.environ.get(key, '')}"]
    if action == "policy":
        # Mount only public authorization policy, never the PAT or signing key.
        if policy:
            args += ["--mount", f"type=bind,source={Path(policy).resolve()},target=/run/policy.json,readonly", "--env", "POLICY_FILE=/run/policy.json"]
    else:
        args += ["--mount", f"type=bind,source={private},target=/run/proxy,readonly",
                 "--env", "GITHUB_TOKEN_FILE=/run/proxy/token", "--env", "TLS_CERT_FILE=/run/proxy/server.pem", "--env", "TLS_KEY_FILE=/run/proxy/server-key.pem"]
        if os.environ.get("MAX_PUSH_BYTES"):
            args += ["--env", "MAX_PUSH_BYTES=" + os.environ["MAX_PUSH_BYTES"]]
        for key in ("POLICY_FILE", "INTERCEPT_CA_CERT_FILE", "INTERCEPT_CA_KEY_FILE"):
            if os.environ.get(key):
                try:
                    relative = Path(os.environ[key]).resolve().relative_to(private)
                except ValueError:
                    raise SystemExit(f"{key} must be inside PROXY_PRIVATE for container recipes.")
                args += ["--env", f"{key}=/run/proxy/{relative}"]
    if action == "run":
        args += [os.environ["PROXY_IMAGE"]]
    elif action == "policy":
        args += ["--env", "POLICY_CHECK=1", "--env", "POLICY_CHECK_REPO=" + sys.argv[2], "--env", "POLICY_CHECK_OWNER_TYPE=" + sys.argv[3],
                 os.environ["TEST_IMAGE"], "go", "test", "-v", "-count=1", "./internal/proxy", "-run", "^TestConfiguredPolicy$"]
    elif action == "live":
        args += [os.environ["TEST_IMAGE"], "bash", "scripts/test-live.sh"]
    else:
        raise SystemExit("unknown action")
    subprocess.run(args, check=True)


if __name__ == "__main__":
    main()
