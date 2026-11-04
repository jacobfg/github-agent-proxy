"""Installed into the companion repo to test the complete trial sandbox."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import unittest

from test_network_profile import NetworkProfileTests
from test_proxy_launcher import ProxyLauncherTests


class TrialLauncherTests(ProxyLauncherTests):
    def test_trial_uses_separate_proxy_config_and_network_profile(self):
        data = self.launch('--proxy-trial', 'true')
        self.assertEqual(data['env']['HTTPS_PROXY'], 'https://127.0.0.1:8444')
        self.assertTrue(data['env']['GIT_CONFIG_GLOBAL'].endswith('/.config/agent-safehouse-trial/git/config'))
        self.assertTrue(data['env']['CURL_HOME'].endswith('/.config/agent-safehouse-trial/curl'))
        self.assertTrue(any(arg.endswith('/99-proxy-trial-network.sb') for arg in data['args']))
        self.assertTrue(any(arg.endswith('/99-proxy-trial-files.sb') for arg in data['args']))
        self.assertFalse(any(arg.endswith('/99-proxy-only-network.sb') for arg in data['args']))
        self.assertEqual(data['args'][-1], 'true')


@unittest.skipUnless(shutil.which('safehouse'), 'requires Safehouse')
class GeneratedTrialNetworkTests(NetworkProfileTests):
    def setUp(self):
        super().setUp()
        root = Path(__file__).resolve().parents[1]
        result = subprocess.run([str(root / 'run-in-sandbox-trial'), '--stdout',
                                 '--workdir=' + self.temp.name, '--', sys.executable],
                                capture_output=True, text=True, check=True, timeout=30)
        self.policy.write_text(result.stdout.replace('localhost:8444', f'localhost:{self.port}'))

    def test_private_proxy_material_denied(self):
        self.run_probe('''
import os
from pathlib import Path
home = Path.home()
paths = [home / '.config/github-agent-proxy-private/token',
         home / '.config/github-agent-proxy-trial-private/intercept-ca-key.pem',
         home / '.config/github-agent-proxy-trial-private/policy.json',
         home / '.config/github-agent-proxy-trial-private/github-agent-proxy']
for path in paths:
    try:
        fd = os.open(path, os.O_RDONLY)
    except PermissionError:
        continue
    except FileNotFoundError:
        continue
    else:
        os.close(fd)
        raise AssertionError('private proxy file accessible: ' + str(path))
''')
