#!/usr/bin/env python3
"""Add an opt-in trial launcher without duplicating local credentials."""
import argparse
from pathlib import Path


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise SystemExit("Launcher differs from the supported layout; no changes written.")
    return text.replace(old, new, 1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("safehouse_repo", type=Path)
    args = parser.parse_args()
    root = args.safehouse_repo.resolve()
    launcher = root / "run-in-sandbox"
    source = launcher.read_text()
    if '# Proxy interception trial selection' not in source:
        source = replace_once(source, 'profiles="$script_dir/profiles"', '''profiles="$script_dir/profiles"
# Proxy interception trial selection; the default service/config stays unchanged.
proxy_port=8443
agent_config="$HOME/.config/agent-safehouse"
proxy_network_profile="$profiles/99-proxy-only-network.sb"
trial_profiles=()
if [[ "${1:-}" == "--proxy-trial" ]]; then
  shift
  proxy_port=8444
  agent_config="$HOME/.config/agent-safehouse-trial"
  proxy_network_profile="$profiles/99-proxy-trial-network.sb"
  trial_profiles+=("--append-profile=$profiles/99-proxy-trial-files.sb")
fi''')
        source = replace_once(source, 'export NETSKOPE_CUSTOM_BUNDLE="$HOME/.nscacert_custom_combined.pem"', '''export NETSKOPE_CUSTOM_BUNDLE="$HOME/.nscacert_custom_combined.pem"
if [[ "$proxy_port" == 8444 ]]; then
  export NETSKOPE_CUSTOM_BUNDLE="$agent_config/ca-bundle.pem"
fi''')
        for key, suffix in [("CURL_HOME", "curl"), ("GH_CONFIG_DIR", "gh"), ("GIT_CONFIG_GLOBAL", "git/config")]:
            source = replace_once(source, f'export {key}="$HOME/.config/agent-safehouse/{suffix}"', f'export {key}="$agent_config/{suffix}"')
        source = replace_once(source, 'export SAFEHOUSE_PROXY_URL=https://127.0.0.1:8443', 'export SAFEHOUSE_PROXY_URL="https://127.0.0.1:$proxy_port"')
        source = replace_once(source, '  --append-profile="$profiles/99-proxy-only-network.sb"', '  "${trial_profiles[@]}" \\\n  --append-profile="$proxy_network_profile"')
    # Validate all input before modifying anything; never print launcher contents.
    network = (root / "profiles/99-proxy-only-network.sb").read_text()
    if 'localhost:8443' not in network:
        raise SystemExit("Expected current proxy-only network profile.")
    launcher.write_text(source)
    (root / "profiles/99-proxy-trial-network.sb").write_text(network.replace("localhost:8443", "localhost:8444"))
    (root / "profiles/99-proxy-trial-files.sb").write_text(''';; Public trust and CLI config only; signing keys and PAT stay inaccessible.
(allow file-read* (home-subpath "/.config/agent-safehouse-trial"))
(deny file-write* (home-subpath "/.config/agent-safehouse-trial"))
(deny file-read* file-write*
    (home-subpath "/.config/github-agent-proxy-trial-private")
    (home-subpath "/.config/github-agent-proxy-private"))
''')
    trial = root / "run-in-sandbox-trial"
    trial.write_text('''#!/bin/bash
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$script_dir/run-in-sandbox" --proxy-trial "$@"
''')
    trial.chmod(0o755)
    tests = root / "tests"
    tests.mkdir(exist_ok=True)
    (tests / "test_proxy_trial.py").write_text((Path(__file__).resolve().parent / "safehouse_trial_tests.py").read_text())
    print(f"Installed {trial}. Existing launcher still defaults to port 8443.")


if __name__ == "__main__":
    main()
