"""Exercise the guest command adapter with ordinary owned subprocess groups."""
import ctypes
import importlib.util
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import contextmanager
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('guest_transport_fixture', Path(__file__).with_name('guest.py'))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


class GuestCommandTests(unittest.TestCase):
    def test_noisy_stdout_and_stderr_are_rejected_before_completion(self):
        for descriptor in [1, 2]:
            with self.subTest(descriptor=descriptor), tempfile.TemporaryDirectory() as directory:
                completed = Path(directory) / 'completed'
                code = (f"import os, pathlib, sys; os.write({descriptor}, b'x' * (4 * 1024 * 1024)); "
                        "pathlib.Path(sys.argv[1]).touch()")
                with self.assertRaisesRegex(RuntimeError, 'output exceeds'):
                    guest.command([sys.executable, '-c', code, completed], timeout=3)
                self.assertFalse(completed.exists(), 'noisy command completed beyond its output limit')

    @contextmanager
    def owned_descendants(self):
        # Adopt only this probe's orphaned descendant so a successful group kill
        # can be reaped and distinguished from a still-running process.
        libc = ctypes.CDLL(None, use_errno=True)
        previous = ctypes.c_int()
        if libc.prctl(37, ctypes.byref(previous), 0, 0, 0) != 0 or libc.prctl(36, 1, 0, 0, 0) != 0:
            raise OSError(ctypes.get_errno(), 'cannot establish test child subreaper')
        try:
            with tempfile.TemporaryDirectory() as directory:
                pidfile = Path(directory) / 'pids.json'
                try:
                    yield pidfile
                finally:
                    if pidfile.exists():
                        pids = json.loads(pidfile.read_text())
                        for pid in [pids['parent'], pids['child']]:
                            try:
                                os.kill(pid, signal.SIGKILL)
                            except ProcessLookupError:
                                pass
                        for pid in [pids['parent'], pids['child']]:
                            try:
                                os.waitpid(pid, 0)
                            except ChildProcessError:
                                pass
        finally:
            if libc.prctl(36, previous.value, 0, 0, 0) != 0:
                raise OSError(ctypes.get_errno(), 'cannot restore test child subreaper')

    def descendant_command(self, pidfile):
        code = ("import json, os, pathlib, subprocess, sys, time; "
                "child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)']); "
                "pathlib.Path(sys.argv[1]).write_text(json.dumps({'parent':os.getpid(), "
                "'child':child.pid, 'group':os.getpgrp()})); time.sleep(30)")
        return [sys.executable, '-c', code, pidfile]

    def assert_group_reaped(self, pidfile):
        pids = json.loads(pidfile.read_text())
        deadline, reaped = time.monotonic() + 1, 0
        while time.monotonic() < deadline:
            reaped, _ = os.waitpid(pids['child'], os.WNOHANG)
            if reaped:
                break
            time.sleep(0.01)
        self.assertEqual(reaped, pids['child'], 'command descendant remains running')
        for pid in [pids['parent'], pids['child']]:
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
            with self.assertRaises(ChildProcessError):
                os.waitpid(pid, os.WNOHANG)
        with self.assertRaises(ProcessLookupError):
            os.killpg(pids['group'], 0)

    def test_timeout_kills_descendants_and_reaps_the_owned_group(self):
        with self.owned_descendants() as pidfile:
            started = time.monotonic()
            with self.assertRaises(subprocess.TimeoutExpired):
                guest.command(self.descendant_command(pidfile), timeout=1)
            self.assertLess(time.monotonic() - started, 3)
            self.assert_group_reaped(pidfile)

    def test_cancellation_kills_descendants_and_reaps_the_owned_group(self):
        def cancel(signum, frame):
            raise KeyboardInterrupt('synthetic guest command cancellation')

        with self.owned_descendants() as pidfile:
            previous = signal.signal(signal.SIGALRM, cancel)
            try:
                signal.setitimer(signal.ITIMER_REAL, 1)
                with self.assertRaises(KeyboardInterrupt):
                    guest.command(self.descendant_command(pidfile), timeout=3)
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, previous)
            self.assert_group_reaped(pidfile)

    def test_adapter_returns_utf8_text_with_the_fixed_guest_environment(self):
        code = ("import json, os, sys; os.write(1, 'grüß\\n'.encode('utf-8')); "
                "print(json.dumps({'stdin':sys.stdin.read(), 'path':os.environ['PATH'], "
                "'locale':os.environ['LC_ALL'], 'colors':os.environ['SYSTEMD_COLORS'], "
                "'pager':os.environ['SYSTEMD_PAGER'], 'columns':os.environ['COLUMNS'], "
                "'inherited':os.environ.get('GUEST_TRANSPORT_PRIVATE_VALUE')}))")
        with patch.dict(os.environ, {'GUEST_TRANSPORT_PRIVATE_VALUE': 'must not inherit'}):
            output = guest.command([sys.executable, '-c', code], timeout=3)
        self.assertIsInstance(output, str)
        greeting, report = output.splitlines()
        self.assertEqual(greeting, 'grüß')
        self.assertEqual(json.loads(report), {'stdin': '', 'path': '/usr/local/bin:/usr/bin',
                         'locale': 'C', 'colors': '0', 'pager': 'cat', 'columns': '4096', 'inherited': None})


if __name__ == '__main__':
    unittest.main()
