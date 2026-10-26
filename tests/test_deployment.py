import hashlib
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]


class InitializationTests(unittest.TestCase):
    def setUp(self):
        self.sandbox = tempfile.TemporaryDirectory()
        self.addCleanup(self.sandbox.cleanup)
        self.root = pathlib.Path(self.sandbox.name)
        (self.root / 'scripts').mkdir()
        shutil.copy2(ROOT / 'scripts/stack.sh', self.root / 'scripts/stack.sh')
        shutil.copy2(ROOT / '.env.example', self.root / '.env.example')

    def init(self):
        return subprocess.run(['bash', 'scripts/stack.sh', 'init'], cwd=self.root, capture_output=True)

    def test_init_is_private_and_idempotent(self):
        self.assertEqual(self.init().returncode, 0)
        paths = [self.root / 'secrets/session_secret', self.root / 'secrets/crypto_secret']
        hashes = [hashlib.sha256(path.read_bytes()).hexdigest() for path in paths]
        self.assertNotEqual(hashes[0], hashes[1])
        for path in paths:
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(len(path.read_text().strip()), 64)
        self.assertEqual((self.root / 'secrets').stat().st_mode & 0o777, 0o700)
        with (self.root / '.env').open('a') as output:
            output.write('\nNEW_API_PORT=23000\n')
        settings = (self.root / '.env').read_bytes()
        self.assertEqual(self.init().returncode, 0)
        self.assertEqual([hashlib.sha256(path.read_bytes()).hexdigest() for path in paths], hashes)
        self.assertEqual((self.root / '.env').read_bytes(), settings)

    def test_symlink_secret_directory_is_rejected(self):
        target = self.root / 'outside'
        target.mkdir()
        (self.root / 'secrets').symlink_to(target, target_is_directory=True)
        self.assertNotEqual(self.init().returncode, 0)
        self.assertEqual(list(target.iterdir()), [])

    def test_invalid_existing_secret_is_not_replaced(self):
        (self.root / 'secrets').mkdir()
        secret = self.root / 'secrets/session_secret'
        secret.write_text('invalid')
        self.assertNotEqual(self.init().returncode, 0)
        self.assertEqual(secret.read_text(), 'invalid')


if __name__ == '__main__':
    unittest.main()

