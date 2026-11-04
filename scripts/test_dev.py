"""Offline checks for trial installation and container argument construction."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]


def module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / (name + ".py"))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


class DevelopmentCommands(unittest.TestCase):
    def test_container_policy_does_not_mount_private_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            policy = Path(temp) / "policy.json"
            policy.write_text('{}')
            env = {"PROXY_PRIVATE": temp, "POLICY_FILE": str(policy), "TEST_IMAGE": "test"}
            with patch.dict(os.environ, env, clear=True), patch('sys.argv', ['script', 'policy', 'acme/repo', 'Organization']), patch('subprocess.run') as run:
                module('container-run').main()
            args = run.call_args.args[0]
            self.assertFalse(any('/run/proxy' in arg or 'TOKEN' in arg or 'KEY_FILE' in arg for arg in args))
            self.assertIn('POLICY_FILE=/run/policy.json', args)

    def test_container_rejects_conflicting_environment(self):
        with patch.dict(os.environ, {"PROXY_PRIVATE": "/tmp", "POLICY_FILE": "/tmp/policy.json", "ALLOWED_ORGS": "acme"}, clear=True), patch('sys.argv', ['script', 'policy', 'acme/repo', 'Organization']):
            with self.assertRaises(SystemExit):
                module('container-run').main()

    def test_trial_install_preserves_unrelated_lines_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'profiles').mkdir()
            (root / 'profiles/99-proxy-only-network.sb').write_text('(allow network-outbound (remote tcp4 "localhost:8443"))\n')
            original = '''#!/bin/bash
profiles="$script_dir/profiles"
export NETSKOPE_CUSTOM_BUNDLE="$HOME/.nscacert_custom_combined.pem"
export CURL_HOME="$HOME/.config/agent-safehouse/curl"
export GH_CONFIG_DIR="$HOME/.config/agent-safehouse/gh"
export GIT_CONFIG_GLOBAL="$HOME/.config/agent-safehouse/git/config"
export SAFEHOUSE_PROXY_URL=https://127.0.0.1:8443
# unrelated local settings must survive
export CUSTOM_SETTING=synthetic-test-sentinel
exec safehouse \\
  --append-profile="$profiles/99-proxy-only-network.sb" \\
  "$@"
'''
            launcher = root / 'run-in-sandbox'
            launcher.write_text(original)
            installer = module('install-safehouse-trial')
            with patch('sys.argv', ['script', str(root)]):
                installer.main()
                first = launcher.read_text()
                installer.main()
            self.assertEqual(first, launcher.read_text())
            self.assertIn('export CUSTOM_SETTING=synthetic-test-sentinel', first)
            self.assertIn('proxy_port=8443', first)
            self.assertIn('proxy_port=8444', first)
            self.assertIn('localhost:8444', (root / 'profiles/99-proxy-trial-network.sb').read_text())
            self.assertIn('deny file-read* file-write*', (root / 'profiles/99-proxy-trial-files.sb').read_text())


if __name__ == '__main__':
    unittest.main()
