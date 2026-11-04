#!/usr/bin/env python3
"""Read-only native trial smoke tests; never print credential-helper output."""
import argparse
import json
import os
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("safehouse_repo", type=Path)
    parser.add_argument("--claude", action="store_true")
    parser.add_argument("--repo", default="jacobfg/permission-test")
    parser.add_argument("--workdir", type=Path)
    args = parser.parse_args()
    wrapper = args.safehouse_repo.resolve() / "run-in-sandbox-trial"
    working = args.workdir or args.safehouse_repo / "permission-test"
    if not working.is_dir():
        working = args.safehouse_repo
    bundle = Path.home() / ".config/agent-safehouse-trial/ca-bundle.pem"
    status = subprocess.run(["curl", "--noproxy", "*", "--cacert", str(bundle), "--fail", "--silent", "--show-error", "https://127.0.0.1:8444/status"], capture_output=True, text=True, timeout=15, check=True)
    policy = json.loads(status.stdout)
    assert policy["non_github_https"] == "intercept" and policy["all_https_tls_enforced"]
    print("PASS: trial status reports interception and TLS 1.2 minimum", flush=True)
    checks = [
        ("gh api user", ["gh", "api", "user", "--jq", ".login"], True),
        ("gh repo view", ["gh", "repo", "view", args.repo, "--json", "nameWithOwner"], True),
        ("git ls-remote", ["git", "ls-remote", "https://github.com/" + args.repo + ".git", "HEAD"], True),
        ("curl public HTTPS", ["curl", "--fail", "--silent", "--show-error", "--max-time", "20", "--output", "/dev/null", "https://example.com"], True),
        ("blocked hostname", ["curl", "--fail", "--silent", "--max-time", "10", "https://blocked.example"], False),
        ("blocked IP", ["curl", "--fail", "--silent", "--max-time", "10", "https://192.0.2.1"], False),
    ]
    if args.workdir:
        checks.insert(2, ("gh repo view from local checkout", ["gh", "repo", "view", "--json", "nameWithOwner"], True))
    for name, command, success in checks:
        result = subprocess.run([str(wrapper), *command], cwd=working, capture_output=True, text=True, timeout=45)
        if (result.returncode == 0) != success:
            # These commands do not return credentials; limit diagnostics anyway.
            raise SystemExit(f"FAIL: {name}: exit {result.returncode}: {result.stderr[-1000:]}")
        print(f"PASS: {name}", flush=True)
    if args.claude:
        result = subprocess.run([str(wrapper), "claude", "-p", "Reply with exactly OK", "--tools", "", "--no-session-persistence", "--max-turns", "1"], cwd=working, capture_output=True, text=True, timeout=120)
        if result.returncode != 0 or result.stdout.strip() != "OK":
            reason = "authentication failed" if "authentication failed" in result.stderr + result.stdout else "request failed or returned an unexpected response"
            raise SystemExit(f"FAIL: Claude: {reason}; helper output suppressed")
        print("PASS: Claude minimal request (credential-helper output suppressed)", flush=True)


if __name__ == "__main__":
    main()
