"""Transport checks use disposable files and ordinary synthetic subprocesses."""
import os
import hashlib
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

from transport import defer_start_signals, fetch_verified, run_command, start_bounded_log


class CommandTests(unittest.TestCase):
    def test_noisy_child_is_rejected_before_finishing_output(self):
        with tempfile.TemporaryDirectory() as directory:
            completed = Path(directory) / 'completed'
            code = "import os, pathlib, sys; os.write(1, b'x' * (4 * 1024 * 1024)); pathlib.Path(sys.argv[1]).touch()"
            with self.assertRaisesRegex(RuntimeError, 'output exceeds'):
                run_command([sys.executable, '-c', code, completed], timeout=3)
            self.assertFalse(completed.exists())

    def test_deadline_reaps_child_even_after_it_closes_output(self):
        code = "import os, time; print(os.getpid(), flush=True); os.close(1); os.close(2); time.sleep(30)"
        started = time.monotonic()
        with self.assertRaises(subprocess.TimeoutExpired) as failure:
            run_command([sys.executable, '-c', code], timeout=0.3)
        self.assertLess(time.monotonic() - started, 3)
        self.assertIsNotNone(failure.exception.output)
        pid = int(failure.exception.output.strip())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)
        with self.assertRaises(ChildProcessError):
            os.waitpid(pid, os.WNOHANG)

    def test_stderr_has_the_same_streaming_limit(self):
        code = "import os; os.write(2, b'x' * (4 * 1024 * 1024))"
        with self.assertRaisesRegex(RuntimeError, 'stderr output exceeds'):
            run_command([sys.executable, '-c', code], timeout=3)

    def test_input_and_both_output_pipes_work_in_a_threaded_parent(self):
        payload = b'payload\n' * 32768
        code = "import os, sys; os.write(2, b'e' * 131072); sys.stdout.buffer.write(sys.stdin.buffer.read())"
        with ThreadPoolExecutor(max_workers=2) as workers:
            tasks = [workers.submit(run_command, [sys.executable, '-c', code], 3, payload) for _ in range(2)]
            self.assertEqual([task.result(timeout=5) for task in tasks], [payload, payload])

    def test_cancellation_reaps_the_owned_child(self):
        with tempfile.TemporaryDirectory() as directory:
            pidfile = Path(directory) / 'child.pid'
            code = "import os, pathlib, sys, time; pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(30)"

            def cancel(signum, frame):
                raise KeyboardInterrupt('synthetic cancellation')

            previous = signal.signal(signal.SIGALRM, cancel)
            try:
                signal.setitimer(signal.ITIMER_REAL, 0.3)
                with self.assertRaises(KeyboardInterrupt):
                    run_command([sys.executable, '-c', code, pidfile], timeout=3)
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, previous)
            pid = int(pidfile.read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
            with self.assertRaises(ChildProcessError):
                os.waitpid(pid, os.WNOHANG)

    def test_signal_during_spawn_is_deferred_until_child_is_owned(self):
        real_popen = subprocess.Popen
        children = []

        def spawn_then_cancel(*args, **kwargs):
            child = real_popen(*args, **kwargs)
            children.append(child)
            os.kill(os.getpid(), signal.SIGTERM)
            return child

        def cancel(signum, frame):
            raise KeyboardInterrupt('synthetic startup cancellation')

        previous = signal.signal(signal.SIGTERM, cancel)
        try:
            with patch('subprocess.Popen', side_effect=spawn_then_cancel):
                with self.assertRaises(KeyboardInterrupt):
                    run_command([sys.executable, '-c', 'import time; time.sleep(30)'], timeout=3)
            self.assertIsNotNone(children[0].returncode)
            with self.assertRaises(ProcessLookupError):
                os.kill(children[0].pid, 0)
        finally:
            signal.signal(signal.SIGTERM, previous)
            for child in children:
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=3)

    def test_child_does_not_inherit_blocked_cancellation_signals(self):
        code = "import signal; print(','.join(str(int(s)) for s in signal.pthread_sigmask(signal.SIG_BLOCK, [])))"
        blocked = run_command([sys.executable, '-c', code], timeout=3).decode().strip().split(',')
        self.assertNotIn(str(int(signal.SIGINT)), blocked)
        self.assertNotIn(str(int(signal.SIGTERM)), blocked)

    def test_child_uses_supplied_directory_and_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            code = "import os; print(os.getcwd()); print(os.environ['TRANSPORT_TEST_VALUE'])"
            result = run_command([sys.executable, '-c', code], cwd=directory,
                                 env={'TRANSPORT_TEST_VALUE': 'selected'}, timeout=3)
            self.assertEqual(result.decode().splitlines(), [directory, 'selected'])


class StartupSignalTests(unittest.TestCase):
    def test_default_termination_is_forwarded_after_owned_assignment(self):
        previous = signal.signal(signal.SIGTERM, signal.SIG_DFL)
        owned = False
        try:
            with self.assertRaises(KeyboardInterrupt):
                with defer_start_signals():
                    os.kill(os.getpid(), signal.SIGTERM)
                    owned = True
            self.assertTrue(owned)
            self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_DFL)
        finally:
            signal.signal(signal.SIGTERM, previous)

    def test_ignored_signal_stays_ignored(self):
        previous = signal.signal(signal.SIGTERM, signal.SIG_IGN)
        try:
            with defer_start_signals():
                os.kill(os.getpid(), signal.SIGTERM)
                self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)
            self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)
        finally:
            signal.signal(signal.SIGTERM, previous)

    def test_direct_signal_deferral_in_worker_thread_is_rejected(self):
        def attempt():
            with defer_start_signals():
                pass
        with ThreadPoolExecutor(max_workers=1) as workers:
            with self.assertRaisesRegex(RuntimeError, 'main thread'):
                workers.submit(attempt).result(timeout=3)


class LogTests(unittest.TestCase):
    def test_log_is_capped_while_full_pipe_is_drained(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'qemu.log'
            code = "import os; os.write(1, b'x' * (4 * 1024 * 1024))"
            with subprocess.Popen([sys.executable, '-c', code], stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT) as child:
                handle = start_bounded_log(child.stdout, target)
                try:
                    self.assertEqual(child.wait(timeout=3), 0)
                    handle.join()
                finally:
                    handle.close()
            self.assertEqual(target.stat().st_size, 2 * 1024 * 1024)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_close_stops_an_idle_pipe_and_existing_evidence_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'qemu.log'
            reader, writer = os.pipe()
            try:
                handle = start_bounded_log(os.fdopen(reader, 'rb'), target, limit=128)
                started = time.monotonic()
                handle.close()
                self.assertLess(time.monotonic() - started, 2)
                target.write_bytes(b'prior evidence')
                with os.fdopen(os.dup(writer), 'wb') as pipe:
                    with self.assertRaises(FileExistsError):
                        start_bounded_log(pipe, target)
                self.assertEqual(target.read_bytes(), b'prior evidence')
            finally:
                os.close(writer)


class DownloadTests(unittest.TestCase):
    def fake_curl(self, root, payload=b'good', status=0, version='8.4.0'):
        curl = root / 'curl'
        curl.write_text('#!' + sys.executable + '\nimport json, pathlib, sys\n'
                        f'if "--version" in sys.argv:\n print("curl {version}")\n sys.exit(0)\n'
                        f'pathlib.Path({str(root / "curl-args.json")!r}).write_text(json.dumps(sys.argv[1:]))\n'
                        f'pathlib.Path(sys.argv[sys.argv.index("--output")+1]).write_bytes({payload!r})\n'
                        f'sys.stderr.write("download failed" if {status} else "")\nsys.exit({status})\n')
        curl.chmod(0o700)

    def test_old_curl_is_rejected_before_creating_a_download(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.fake_curl(root, version='8.3.0')
            target = root / 'image'
            asset = {'url': 'https://synthetic.invalid/image', 'sha256': hashlib.sha256(b'good').hexdigest()}
            with patch.dict(os.environ, {'PATH': str(root)}):
                with self.assertRaisesRegex(RuntimeError, 'curl 8.4.0'):
                    fetch_verified(asset, target, 128)
            self.assertFalse(target.exists())

    def test_failing_downloader_removes_partial_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.fake_curl(root, b'partial', 22)
            target = root / 'image'
            asset = {'url': 'https://synthetic.invalid/image', 'sha256': hashlib.sha256(b'good').hexdigest()}
            with patch.dict(os.environ, {'PATH': str(root)}):
                with self.assertRaisesRegex(RuntimeError, 'curl exited 22'):
                    fetch_verified(asset, target, 128)
            self.assertFalse(target.exists())

    def test_verified_download_is_private_and_passes_hard_curl_budgets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.fake_curl(root)
            target = root / 'image'
            asset = {'url': 'https://synthetic.invalid/image', 'sha256': hashlib.sha256(b'good').hexdigest()}
            with patch.dict(os.environ, {'PATH': str(root)}):
                self.assertEqual(fetch_verified(asset, target, 128), target)
            self.assertEqual(target.read_bytes(), b'good')
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            import json
            arguments = json.loads((root / 'curl-args.json').read_text())
            self.assertEqual(arguments[0], '--disable')
            for flag, value in [('--max-time', '600'), ('--max-filesize', '128'),
                                ('--proto', '=https'), ('--proto-redir', '=https')]:
                self.assertEqual(arguments[arguments.index(flag) + 1], value)

    def test_checksum_mismatch_and_oversized_download_are_removed(self):
        for payload, message in [(b'wrong', 'SHA-256 mismatch'), (b'x' * 129, 'asset limit')]:
            with self.subTest(message=message), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                self.fake_curl(root, payload)
                target = root / 'image'
                asset = {'url': 'https://synthetic.invalid/image', 'sha256': hashlib.sha256(b'good').hexdigest()}
                with patch.dict(os.environ, {'PATH': str(root)}):
                    with self.assertRaisesRegex(RuntimeError, message):
                        fetch_verified(asset, target, 128)
                self.assertFalse(target.exists())

    def test_existing_download_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'image'
            target.write_bytes(b'prior evidence')
            asset = {'url': 'https://synthetic.invalid/image', 'sha256': hashlib.sha256(b'good').hexdigest()}
            with self.assertRaises(FileExistsError):
                fetch_verified(asset, target, 128)
            self.assertEqual(target.read_bytes(), b'prior evidence')


if __name__ == '__main__':
    unittest.main()
