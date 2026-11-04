#!/usr/bin/env python3
"""Local native trial. Never changes or stops the existing 8443 service."""
import argparse
import json
import os
from pathlib import Path
import plistlib
import socket
import subprocess

ROOT = Path(__file__).resolve().parents[1]
HOME_DIR = Path.home()
LABEL = "local.github-agent-proxy-trial"
DOMAIN = f"gui/{os.getuid()}"
PRIVATE = HOME_DIR / ".config/github-agent-proxy-trial-private"
PUBLIC = HOME_DIR / ".config/agent-safehouse-trial"
SOURCE = Path(os.environ.get("PROXY_PRIVATE", HOME_DIR / ".config/github-agent-proxy-private"))


def write(path, contents, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_bytes(contents if isinstance(contents, bytes) else contents.encode())
    path.chmod(mode)


def source_environment():
    source = SOURCE / "host-proxy.plist"
    if not source.is_file():
        raise SystemExit("Native trial requires the existing host-proxy.plist; start/configure the normal host proxy first.")
    return plistlib.loads(source.read_bytes())["EnvironmentVariables"]


def prepare():
    PRIVATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    PRIVATE.chmod(0o700)
    source = source_environment()
    binary = PRIVATE / "github-agent-proxy.new"
    subprocess.run(["go", "build", "-trimpath", "-o", str(binary), "./cmd/github-agent-proxy"], cwd=ROOT, check=True)
    binary.chmod(0o700)
    binary.replace(PRIVATE / "github-agent-proxy")
    ca, key = PRIVATE / "intercept-ca.pem", PRIVATE / "intercept-ca-key.pem"
    if not ca.exists() and not key.exists():
        subprocess.run([str(PRIVATE / "github-agent-proxy"), "init-ca", "--cert", str(ca), "--key", str(key)], check=True)
    if not ca.is_file() or not key.is_file():
        raise SystemExit("Incomplete trial CA pair; restore the missing file before starting.")
    policy = PRIVATE / "policy.json"
    if not policy.exists():
        if source.get("POLICY_FILE"):
            config = json.loads(Path(source["POLICY_FILE"]).read_text())
        else:
            config = {"github": {
                "allowed_orgs": [s.strip() for s in source.get("ALLOWED_ORGS", "").split(",") if s.strip()],
                "allowed_users": [s.strip() for s in source.get("ALLOWED_USERS", "").split(",") if s.strip()],
                "branch_prefix": source["BRANCH_PREFIX"]}}
        config["https_mode"] = "intercept"
        config.setdefault("destinations", {"deny_hosts": ["blocked.example"], "deny_ips": ["192.0.2.0/24", "2001:db8::/32"]})
        write(policy, json.dumps(config, indent=2) + "\n")
    # Trust is scoped to this sandbox; do not install the new CA in Keychain.
    base = Path(os.environ.get("TRIAL_BASE_CA_BUNDLE", HOME_DIR / ".nscacert_custom_combined.pem"))
    if not base.is_file():
        raise SystemExit("Set TRIAL_BASE_CA_BUNDLE to a bundle containing public/corporate roots and the existing listener CA.")
    bundle = PUBLIC / "ca-bundle.pem"
    write(bundle, base.read_bytes() + b"\n" + ca.read_bytes(), 0o644)
    git_config = PUBLIC / "git/config"
    if not git_config.exists():
        original = HOME_DIR / ".config/agent-safehouse/git/config"
        write(git_config, original.read_bytes() if original.exists() else b"")
    for key_name, value in {
        "http.proxy": "https://127.0.0.1:8444",
        "http.https://github.com/.proxy": "https://127.0.0.1:8444",
        "http.sslCAInfo": str(bundle), "http.proxySSLCAInfo": str(bundle),
        "credential.https://github.com.helper": "",
        "url.https://github.com/.insteadOf": "git@github.com:",
    }.items():
        subprocess.run(["git", "config", "--file", str(git_config), "--replace-all", key_name, value], check=True)
    subprocess.run(["git", "config", "--file", str(git_config), "--add", "url.https://github.com/.insteadOf", "ssh://git@github.com/"], check=True)
    write(PUBLIC / "curl/.curlrc", 'proxy-cacert = ' + json.dumps(str(bundle)) + '\n', 0o644)
    (PUBLIC / "gh").mkdir(exist_ok=True)
    environment = {k: v for k, v in source.items() if k in {
        "GITHUB_TOKEN_FILE", "GITHUB_TOKEN", "TLS_CERT_FILE", "TLS_KEY_FILE", "MAX_PUSH_BYTES", "SSL_CERT_FILE", "SSL_CERT_DIR"}}
    environment.update(LISTEN_ADDR="127.0.0.1:8444", POLICY_FILE=str(policy), INTERCEPT_CA_CERT_FILE=str(ca), INTERCEPT_CA_KEY_FILE=str(PRIVATE / "intercept-ca-key.pem"))
    log = PRIVATE / "host-proxy.log"
    log.touch(mode=0o600, exist_ok=True)
    config = {"Label": LABEL, "ProgramArguments": [str(PRIVATE / "github-agent-proxy")],
              "WorkingDirectory": str(PRIVATE), "EnvironmentVariables": environment,
              "StandardOutPath": str(log), "StandardErrorPath": str(log),
              "RunAtLoad": True, "KeepAlive": {"SuccessfulExit": False}, "ThrottleInterval": 10}
    write(PRIVATE / "host-proxy.plist", plistlib.dumps(config))
    print(f"Trial prepared. Policy: {policy}. Public trust bundle: {bundle}.")


def stop():
    service = f"{DOMAIN}/{LABEL}"
    if subprocess.run(["launchctl", "print", service], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
        subprocess.run(["launchctl", "bootout", service], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["prepare", "start", "stop", "status"])
    args = parser.parse_args()
    if args.action in ("prepare", "start"):
        prepare()
    if args.action == "start":
        stop()
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 8444))
        subprocess.run(["launchctl", "bootstrap", DOMAIN, str(PRIVATE / "host-proxy.plist")], check=True)
        print("Trial launched on https://127.0.0.1:8444; existing 8443 service unchanged.")
    elif args.action == "stop":
        stop()
    elif args.action == "status":
        subprocess.run(["curl", "--noproxy", "*", "--cacert", str(PUBLIC / "ca-bundle.pem"), "--fail", "--silent", "--show-error", "https://127.0.0.1:8444/status"], check=True)


if __name__ == "__main__":
    main()
