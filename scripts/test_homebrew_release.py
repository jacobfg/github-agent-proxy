"""Release invariants: fail closed on corrupt, wrong-version or incomplete bottles."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import homebrew_release as release


class HomebrewReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.template = (release.ROOT / 'Formula/github-agent-proxy.rb').read_text()

    def test_render_preserves_formula_and_discards_stale_bottles(self):
        template = self.template.replace('  depends_on :macos', '  bottle do\n    sha256 arm64_sequoia: "old"\n  end\n\n  depends_on :macos')
        result = release.render_formula(template, '1.2.3', 'a' * 64)
        self.assertIn('version "1.2.3"', result)
        self.assertIn('releases/download/1.2.3/github-agent-proxy-1.2.3-source.tar.gz', result)
        self.assertNotIn('bottle do', result)
        self.assertIn('depends_on "go" => :build', result)
        self.assertIn('def install', result)
        self.assertNotIn('service do', result)
        self.assertNotIn('post_install', result)

    def test_versions_and_source_hash_cannot_inject_formula_code(self):
        for value in ['v1.2.3', '1.2', '01.2.3', '../main', '1.2.3\nend', '1.2.3-rc1']:
            with self.assertRaises(ValueError):
                release.render_formula(self.template, value, 'a' * 64)
        with self.assertRaises(ValueError):
            release.render_formula(self.template, '1.2.3', 'bad')

    def source_fixture(self):
        archive = self.path / 'github-agent-proxy-1.2.3-source.tar.gz'
        archive.write_bytes(b'source fixture')
        digest = release.digest(archive)
        info = {'version': '1.2.3', 'commit': 'a' * 40, 'release': True,
                'source': archive.name, 'sha256': digest, 'source_url': release.source_url('1.2.3')}
        (self.path / 'source.json').write_text(json.dumps(info))
        (self.path / 'github-agent-proxy.rb').write_text(release.render_formula(self.template, '1.2.3', digest))
        return archive

    def test_source_integrity_and_formula_integrity(self):
        archive = self.source_fixture()
        self.assertEqual(release.verify_source(self.path)['version'], '1.2.3')
        archive.write_bytes(b'corrupted')
        with self.assertRaisesRegex(ValueError, 'checksum'):
            release.verify_source(self.path)
        self.source_fixture()
        (self.path / 'github-agent-proxy.rb').write_text('untrusted formula')
        with self.assertRaisesRegex(ValueError, 'template'):
            release.verify_source(self.path)

    def bottle_fixture(self):
        for tag in ['arm64_sequoia', 'sequoia']:
            local = f'github-agent-proxy--1.2.3.{tag}.bottle.tar.gz'
            public = local.replace('--', '-')
            body = tag.encode()
            (self.path / local).write_bytes(body)
            metadata = {'jacobfg/github-agent-proxy/github-agent-proxy': {
                'formula': {'name': 'github-agent-proxy', 'pkg_version': '1.2.3'},
                'bottle': {'root_url': 'https://github.com/jacobfg/github-agent-proxy/releases/download/1.2.3',
                           'tags': {tag: {'local_filename': local, 'filename': public,
                                          'sha256': hashlib.sha256(body).hexdigest()}}}}}
            (self.path / (local.removesuffix('.tar.gz') + '.json')).write_text(json.dumps(metadata))

    def test_bottle_matrix_and_hashes_are_checked(self):
        self.bottle_fixture()
        self.assertEqual(len(release.bottle_assets(self.path, '1.2.3')), 2)
        with self.assertRaisesRegex(ValueError, 'version'):
            release.bottle_assets(self.path, '1.2.4')
        (self.path / 'github-agent-proxy--1.2.3.sequoia.bottle.tar.gz').write_bytes(b'corrupted')
        with self.assertRaisesRegex(ValueError, 'checksum'):
            release.bottle_assets(self.path, '1.2.3')
        self.bottle_fixture()
        (self.path / 'github-agent-proxy--1.2.3.sequoia.bottle.json').unlink()
        with self.assertRaisesRegex(ValueError, 'per macOS architecture'):
            release.bottle_assets(self.path, '1.2.3')

    def test_bottle_paths_cannot_escape_artifact_directory(self):
        self.bottle_fixture()
        path = self.path / 'github-agent-proxy--1.2.3.sequoia.bottle.json'
        original = json.loads(path.read_text())
        for field in ['filename', 'local_filename']:
            changed = copy.deepcopy(original)
            changed['jacobfg/github-agent-proxy/github-agent-proxy']['bottle']['tags']['sequoia'][field] = '../escape.tar.gz'
            path.write_text(json.dumps(changed))
            with self.assertRaisesRegex(ValueError, 'filename'):
                release.bottle_assets(self.path, '1.2.3')

    def test_development_artifacts_cannot_publish(self):
        from unittest.mock import patch
        import homebrew_publish as publish
        self.source_fixture()
        path = self.path / 'source.json'
        info = json.loads(path.read_text())
        info['release'] = False
        path.write_text(json.dumps(info))
        with patch('sys.argv', ['script', str(self.path), str(self.path)]), patch.object(publish, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'development builds'):
                publish.main()
            run.assert_not_called()

    def test_changed_tag_cannot_publish(self):
        from unittest.mock import patch
        import homebrew_publish as publish
        self.source_fixture()
        self.bottle_fixture()
        with patch('sys.argv', ['script', str(self.path), str(self.path)]), patch.object(publish, 'run') as run, patch.object(publish, 'output', return_value='b' * 40):
            with self.assertRaisesRegex(ValueError, 'tag changed'):
                publish.main()
            run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
