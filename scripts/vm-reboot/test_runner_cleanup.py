"""Exercise runner cleanup through a disposable, synthetic VM boundary."""
import copy
import hashlib
import importlib.util
import io
import json
import os
import signal
import sys
import tempfile
import unittest
from contextlib import ExitStack, contextmanager, redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('cleanup_runner', Path(__file__).resolve().parents[1] / 'verify-vm-reboot.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class RunnerCleanupTests(unittest.TestCase):
    @contextmanager
    def fixture(self, *, failure=None, close_error=None):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            root = base / 'owned-vm'
            root.mkdir(mode=0o700)
            image, herdr, candidate = [base / name for name in ['image', 'herdr', 'candidate']]
            for path in [image, herdr, candidate]:
                path.write_bytes(b'bounded synthetic artifact')
            fixture = base / 'fixture'
            fixture.mkdir()
            (fixture / 'assets.json').write_text(json.dumps({
                'arch': {'sha256': hashlib.sha256(image.read_bytes()).hexdigest()}, 'herdr': {},
            }))
            for name in ['guest.py', 'transport.py']:
                (fixture / name).write_text('# synthetic guest upload\n')
            build = {'product_version': 'dev', 'commit': 'b' * 40, 'modified': True}
            artifact = {'sha256': hashlib.sha256(candidate.read_bytes()).hexdigest(), 'build': build}
            layout = {'workspaces': [{'name': '98', 'restore_mode': 'layout',
                      'tiling': {'layout': 'tabbed', 'children': [{'context_id': 'a'}, {'context_id': 'b'}]},
                      'floating': []}]}
            uploads = []

            class Log:
                def close(self):
                    if close_error is not None:
                        raise close_error

            class SyntheticVM:
                # No child or VM is started. Keep the real close path so log
                # errors propagate exactly as they do after QEMU has exited.
                close = runner.Guest.close

                def __init__(self, directory, image, run_id):
                    self.root, self.run_id, self.boot = directory, run_id, 0
                    self.process, self.log = None, Log()
                    for name in ['disk.qcow2', 'seed.iso', 'key']:
                        (directory / name).write_bytes(b'owned disposable resource')

                def start(self):
                    pass

                def wait(self, old_boot=None, timeout=0):
                    if old_boot is not None:
                        self.boot += 1
                    return f'boot-{self.boot}'

                def upload(self, local, target):
                    uploads.append((Path(local), target))

                def ssh(self, request, **kwargs):
                    if request.endswith(' baseline') and failure is not None:
                        raise failure
                    if request.endswith(' baseline') or request.endswith(' observe'):
                        return json.dumps({'run_id': self.run_id, 'boot_id': f'boot-{self.boot}',
                            'live': copy.deepcopy(layout), 'stored': copy.deepcopy(layout),
                            'contexts': [{'id': 'a', 'state': 'active'}, {'id': 'b', 'state': 'active'}],
                            'staging_managed_windows': 0, 'daemon_binary_current': True,
                            'delay_inhibitor': True, 'artifact': artifact}).encode()
                    return b'bounded synthetic guest log'

            def command(argv, **kwargs):
                if argv[:2] == ['qemu-img', 'info']:
                    return json.dumps([{'format': 'qcow2', 'virtual-size': 1024}]).encode()
                if argv[0] == candidate:
                    return json.dumps({'build': build}).encode()
                return b'synthetic QEMU version\n'

            output, stderr = base / 'evidence', io.StringIO()
            arguments = ['review-probe', '--image', str(image), '--herdr', str(herdr),
                         '--candidate', str(candidate), '--output', str(output)]
            previous_umask = os.umask(0o077)
            previous_signal = signal.getsignal(signal.SIGTERM)
            try:
                with ExitStack() as stack:
                    for obj, name, value in [
                        (runner, 'FIXTURE', fixture), (runner, 'Guest', SyntheticVM),
                        (runner, 'command', command), (runner, 'qmp', lambda *args: {'enabled': True}),
                        (runner.os, 'access', lambda *args: True),
                        (runner.shutil, 'which', lambda *args: 'synthetic-tool'),
                        (runner.tempfile, 'mkdtemp', lambda **kwargs: str(root)),
                        (runner.time, 'sleep', lambda seconds: None),
                        (runner.sys, 'argv', arguments),
                    ]:
                        stack.enter_context(patch.object(obj, name, value))
                    stack.enter_context(redirect_stdout(io.StringIO()))
                    stack.enter_context(redirect_stderr(stderr))
                    yield root, output, uploads, stderr
            finally:
                os.umask(previous_umask)
                signal.signal(signal.SIGTERM, previous_signal)

    def test_log_close_error_still_removes_owned_files_and_preserves_primary_failure(self):
        failure = RuntimeError('synthetic baseline failure')
        close_error = OSError('synthetic log write failure')
        with self.fixture(failure=failure, close_error=close_error) as (root, output, _, _):
            with self.assertRaises(Exception) as raised:
                runner.main()
            self.assertIs(raised.exception, failure)
            self.assertFalse(root.exists(), 'disk, seed and key survived failed cleanup')
            report = json.loads((output / 'failure.json').read_text())
            self.assertEqual(report['failure'], str(failure))
            cleanup = json.loads((output / 'cleanup.json').read_text())
            self.assertEqual(cleanup['failure'], str(failure))
            self.assertEqual(cleanup['cleanup_errors'], [{'operation': 'close guest', 'error': str(close_error)}])

    def test_close_error_after_success_fails_the_run_and_removes_owned_files(self):
        with self.fixture(close_error=OSError('synthetic log write failure')) as (root, output, _, _):
            with self.assertRaisesRegex(RuntimeError, 'owned VM cleanup failed'):
                runner.main()
            self.assertFalse(root.exists())
            cleanup = json.loads((output / 'cleanup.json').read_text())
            self.assertIsNone(cleanup['failure'])
            self.assertEqual(cleanup['cleanup_errors'][0]['operation'], 'close guest')

    def test_cancellation_is_preserved_after_log_close_error(self):
        cancellation = KeyboardInterrupt('synthetic cancellation')
        with self.fixture(failure=cancellation, close_error=OSError('synthetic log write failure')) as (root, output, _, _):
            with self.assertRaises(KeyboardInterrupt) as raised:
                runner.main()
            self.assertIs(raised.exception, cancellation)
            self.assertFalse(root.exists())
            cleanup = json.loads((output / 'cleanup.json').read_text())
            self.assertEqual(cleanup['failure'], str(cancellation))

    def test_cleanup_report_write_failure_preserves_primary_failure_and_reports_errors(self):
        failure = RuntimeError('synthetic baseline failure')
        original_open = open

        def reject_cleanup_report(path, *args, **kwargs):
            if Path(path).name == 'cleanup.json':
                raise OSError('synthetic cleanup report write failure')
            return original_open(path, *args, **kwargs)

        with self.fixture(failure=failure, close_error=OSError('synthetic log write failure')) as (root, output, _, stderr):
            with patch('builtins.open', reject_cleanup_report), self.assertRaises(RuntimeError) as raised:
                runner.main()
            self.assertIs(raised.exception, failure)
            self.assertFalse(root.exists())
            self.assertIn('synthetic log write failure', stderr.getvalue())
            self.assertIn('synthetic cleanup report write failure', stderr.getvalue())
            self.assertEqual(json.loads((output / 'failure.json').read_text())['failure'], str(failure))

    def test_shared_transport_is_uploaded_to_the_fixed_guest_directory(self):
        with self.fixture() as (root, _, uploads, _):
            runner.main()
            self.assertFalse(root.exists())
            self.assertIn((runner.FIXTURE / 'transport.py', '/opt/sway-session-test/transport.py'), uploads)


if __name__ == '__main__':
    unittest.main()
