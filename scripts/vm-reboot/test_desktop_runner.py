"""Synthetic desktop transport/cleanup tests; no VM, apps or network are used."""
import copy
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import ExitStack, contextmanager, redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import desktop_runner as desktop
import test_desktop_evidence as reports
import test_runner as terminal_reports

SPEC = importlib.util.spec_from_file_location(
    'desktop_transport_test_vm_runner', Path(__file__).resolve().parents[1] / 'verify-vm-reboot.py')
runner = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = runner
SPEC.loader.exec_module(runner)
PRIVATE = 'private-browser-title-url-argv-environment-secret'


class JSONAndCheckpoints(unittest.TestCase):
    def test_json_rejects_duplicate_nonfinite_fractional_and_oversized_data(self):
        for data in [b'{"phase":1,"phase":2}', b'{"nested":{"a":1,"a":2}}',
                     b'{"x":NaN}', b'{"x":Infinity}', b'{"x":-Infinity}',
                     b'{"x":1e999}', b'{"x":-1e999}', b'{"x":0.5}', b'{"x":1.0}',
                     b'{', b'\xff', None]:
            with self.subTest(data=data):
                with self.assertRaises(RuntimeError) as caught:
                    desktop.read_report(data)
                self.assertLess(len(str(caught.exception)), 100)
        self.assertEqual(desktop.read_report(b'{"count":2,"ok":true}'), {'count': 2, 'ok': True})
        for data in [b' ' * 65, ' ' * 65, '"' + '\u00e9' * 32 + '"']:
            with self.subTest(data_type=type(data)), self.assertRaises(RuntimeError):
                desktop.read_report(data, limit=64)

    def test_file_reports_read_only_the_byte_budget_plus_one(self):
        for payload, accepted in [(b'{"ok":true}', True), (b' ' * 128, False)]:
            reads = []

            class BoundedInput(io.BytesIO):
                def read(self, size=-1):
                    reads.append(size)
                    self_test.assertGreaterEqual(size, 0, 'unbounded evidence file read')
                    self_test.assertLessEqual(size, 65)
                    return super().read(size)

            self_test = self
            with patch.object(Path, 'open', return_value=BoundedInput(payload)) as opened:
                if accepted:
                    self.assertEqual(desktop.read_report_file(Path('/synthetic/report.json'), limit=64), {'ok': True})
                else:
                    with self.assertRaises(RuntimeError):
                        desktop.read_report_file(Path('/synthetic/report.json'), limit=64)
                opened.assert_called_once_with('rb')
                self.assertEqual(reads, [65])

    def checkpoint(self):
        return {'id': 'gui.chrome.approve', 'kind': 'approval',
                'private_screenshot': '/home/ss-test/.cache/lab253/approve.png',
                'actions': ['cancel', 'approve']}

    def test_checkpoint_request_limits_private_paths_actions_and_fields(self):
        value = self.checkpoint()
        self.assertEqual(desktop.checkpoint_request(value), value)
        for replacement in [
                {'id': '../../outside'}, {'id': 1}, {'id': 'x' * 65}, {'kind': 'shell'},
                {'actions': ['exec']}, {'actions': ['approve', 'approve']}, {'actions': []},
                {'actions': [True]}, {'private_screenshot': '/home/markus/.config/browser.png'},
                {'private_screenshot': '/home/ss-test/.cache/lab253/../browser.png'},
                {'private_screenshot': '/home/ss-test/.cache/lab253/approve.png; echo secret'},
                {'argv': PRIVATE}]:
            value = self.checkpoint()
            value.update(replacement)
            with self.subTest(replacement=replacement), self.assertRaises(RuntimeError):
                desktop.checkpoint_request(value)
        for removed in ['id', 'kind', 'private_screenshot', 'actions']:
            value = self.checkpoint()
            del value[removed]
            with self.assertRaises(RuntimeError):
                desktop.checkpoint_request(value)

    def test_visual_roundtrip_binds_response_and_quotes_only_validated_actions(self):
        checkpoint = self.checkpoint()
        operation = 'python3 /opt/sway-session-test/desktop_guest.py phases'
        calls = []
        complete = {'complete': True, 'phases': reports.fixture()['phases'],
                    'expected_ids': [reports.CHROME, reports.SLACK]}
        peer = SimpleNamespace(process=SimpleNamespace(poll=lambda: None))

        def ssh(command, **kwargs):
            calls.append(command)
            if command == operation:
                return json.dumps({'complete': False, 'checkpoint': checkpoint}).encode()
            if command.startswith('cat -- '):
                return b'\x89PNG\r\n\x1a\nsynthetic-private-pixels'
            return json.dumps(complete).encode()

        peer.ssh = ssh
        original_save = runner.save
        with tempfile.TemporaryDirectory(prefix='lab253-controller-test-') as directory:
            control = Path(directory)

            def controller_reply(path, value):
                original_save(path, value)
                (control / 'response.json').write_text(json.dumps({
                    'checkpoint': checkpoint['id'], 'action': 'approve', 'x': 100, 'y': 20}))

            with patch.object(runner, 'save', controller_reply), \
                    patch.object(desktop, 'read_report_file', wraps=desktop.read_report_file) as read_file, \
                    redirect_stdout(io.StringIO()):
                result = desktop.collect_phases(runner, peer, control, reports.RUN)
            read_file.assert_called_once_with(control / 'response.json', limit=4096)
            self.assertEqual(result, complete)
            self.assertEqual(calls, [operation,
                'cat -- /home/ss-test/.cache/lab253/approve.png',
                operation + ' --checkpoint gui.chrome.approve --action approve --visually-inspected --x 100 --y 20'])
            request = json.loads((control / 'request.json').read_text())
            self.assertEqual(request, {'run_id': reports.RUN, 'checkpoint': checkpoint, 'screenshot': 'screen.png'})
            self.assertEqual({path.name for path in control.iterdir()}, {'request.json', 'response.json', 'screen.png'})

    def test_response_binds_exact_checkpoint_and_integer_output_coordinates(self):
        checkpoint = self.checkpoint()
        accepted = {'checkpoint': checkpoint['id'], 'action': 'approve', 'x': 1279, 'y': 719}
        self.assertEqual(desktop.checkpoint_response(accepted, checkpoint), accepted)
        for replacement in [{'checkpoint': 'gui.slack.approve'}, {'action': 'exec'},
                            {'action': 'not-exposed'}, {'x': True}, {'y': False},
                            {'x': 1.0}, {'x': -1}, {'x': 1280}, {'y': 720},
                            {'y': None}, {'command': PRIVATE}]:
            value = dict(accepted, **replacement)
            with self.subTest(replacement=replacement), self.assertRaises(RuntimeError):
                desktop.checkpoint_response(value, checkpoint)
        for removed in ['checkpoint', 'action', 'x', 'y']:
            value = dict(accepted)
            del value[removed]
            with self.assertRaises(RuntimeError):
                desktop.checkpoint_response(value, checkpoint)
        capability = dict(checkpoint, kind='capability', actions=['not-exposed', 'ui-input'])
        absence = {'checkpoint': checkpoint['id'], 'action': 'not-exposed'}
        self.assertEqual(desktop.checkpoint_response(absence, capability), absence)
        with self.assertRaises(RuntimeError):
            desktop.checkpoint_response(dict(absence, x=1, y=1), capability)

    def test_capability_approval_acknowledges_inspection_without_pointer_input(self):
        checkpoint = dict(self.checkpoint(), kind='capability', actions=['approve', 'ui-input'])
        response = {'checkpoint': checkpoint['id'], 'action': 'approve'}
        self.assertEqual(desktop.checkpoint_response(response, checkpoint), response)
        with self.assertRaises(RuntimeError):
            desktop.checkpoint_response(dict(response, x=100, y=20), checkpoint)
        with self.assertRaises(RuntimeError):
            desktop.checkpoint_response(response, self.checkpoint())


class DesktopTransport(unittest.TestCase):
    @contextmanager
    def flow(self, *, failure=None, close_error=None, remove_error=None, malformed_phase=False):
        """Exercise the real run_desktop orchestration and real evidence checker."""
        with tempfile.TemporaryDirectory(prefix='lab253-runner-test-') as directory:
            base = Path(directory)
            output, control, root = (base / name for name in ['evidence', 'control', 'owned-vm'])
            root.mkdir(mode=0o700)
            fixtures = base / 'fixtures'
            fixtures.mkdir()
            bundle = reports.fixture()
            assets = {'chrome': {'sha256': bundle['app_assets']['chrome']['sha256']}}
            (fixtures / 'assets.json').write_text(json.dumps(assets))
            chrome = base / 'chrome.deb'
            chrome.write_bytes(b'synthetic vendor package')
            events, peers = [], []
            original_save, original_remove = runner.save, desktop.shutil.rmtree

            class SyntheticGuest:
                def __init__(self, directory, image, run_id, *, scenario):
                    self.root, self.run_id, self.index = directory, run_id, 0
                    self.closed = False
                    self.process = SimpleNamespace(poll=lambda: None)
                    self.requests = []
                    peers.append(self)
                    assert scenario == 'desktop'
                    # A raw browser-shaped stream must stay within the private
                    # disposable directory and never be copied into evidence.
                    (directory / 'serial.log').write_text(PRIVATE)

                def start(self):
                    events.append('start')

                def wait(self, *args, **kwargs):
                    return reports.BOOTS[self.index]

                def upload(self, local, target):
                    assert target.startswith('/opt/sway-session-test/')

                def ssh(self, request, **kwargs):
                    self.requests.append(request)
                    if 'desktop_guest.py install ' in request:
                        return json.dumps({'installed': True, 'app_assets': bundle['app_assets']}).encode()
                    if request == 'python3 /opt/sway-session-test/desktop_guest.py phases':
                        if failure:
                            raise failure
                        phases = copy.deepcopy(bundle['phases'])
                        if malformed_phase:
                            phases['gui.chrome.approve']['facts']['raw_browser_output'] = PRIVATE
                        return json.dumps({'complete': True, 'phases': phases,
                                           'expected_ids': [reports.CHROME, reports.SLACK]}).encode()
                    if 'desktop_boot.py baseline ' in request or request.endswith('desktop_boot.py observe'):
                        return json.dumps(bundle['observations'][self.index]).encode()
                    return b''

                def close(self):
                    events.append('close')
                    self.closed = True
                    if close_error:
                        raise close_error

            def prepare(args, directory, manifest, evidence):
                for key in ['artifact', 'base_image_sha256', 'herdr_sha256', 'qemu_version']:
                    evidence[key] = copy.deepcopy(bundle[key])
                return tuple(base / name for name in ['image', 'herdr', 'candidate', 'observer'])

            def reboot(peer, before, index):
                assert before == reports.BOOTS[index - 1]
                peer.index = index
                events.append('reboot' + str(index))
                return copy.deepcopy(bundle['reboot_requests'][index - 1])

            def save(path, value):
                if Path(path).name == 'result.json':
                    self.assertTrue(peers[0].closed)
                    self.assertFalse(root.exists(), 'result was exported before VM-root removal')
                    self.assertFalse(control.exists(), 'result was exported before private-control removal')
                    events.append('result')
                original_save(path, value)

            def remove(path, *args, **kwargs):
                if Path(path) in [root, control]:
                    events.append('remove-' + Path(path).name)
                    if remove_error == Path(path).name:
                        raise OSError(PRIVATE)
                return original_remove(path, *args, **kwargs)

            args = SimpleNamespace(check_evidence=None, output=output, desktop_control=control, chrome=chrome)
            previous_umask = os.umask(0o077)
            try:
                with ExitStack() as stack:
                    for target, name, replacement in [
                            (runner, 'FIXTURE', fixtures), (runner, 'Guest', SyntheticGuest),
                            (runner, 'prepare_artifacts', prepare), (runner, 'request_reboot', reboot),
                            (runner, 'digest', lambda path: assets['chrome']['sha256']),
                            (runner, 'qmp', lambda *args: {'enabled': True}), (runner, 'save', save),
                            (desktop.os, 'access', lambda *args: True),
                            (desktop.shutil, 'which', lambda *args: 'synthetic-tool'),
                            (desktop.shutil, 'rmtree', remove),
                            (desktop.platform, 'machine', lambda: 'x86_64'),
                            (desktop.tempfile, 'mkdtemp', lambda **kwargs: str(root)),
                            (desktop.uuid, 'uuid4', lambda: reports.RUN)]:
                        stack.enter_context(patch.object(target, name, replacement))
                    stdout, stderr = io.StringIO(), io.StringIO()
                    stack.enter_context(redirect_stdout(stdout))
                    stack.enter_context(redirect_stderr(stderr))
                    yield args, root, control, output, events, stdout, stderr, peers
            finally:
                os.umask(previous_umask)

    def test_success_exports_only_structural_files_after_owned_cleanup(self):
        with self.flow() as (args, root, control, output, events, stdout, stderr, peers):
            desktop.run_desktop(runner, args)
            self.assertEqual(events, ['start', 'reboot1', 'reboot2', 'close', 'remove-owned-vm', 'remove-control', 'result'])
            self.assertEqual({path.name for path in output.iterdir()}, {'before.json', 'after1.json', 'after2.json', 'result.json'})
            result = json.loads((output / 'result.json').read_text())
            self.assertTrue(result['checks']['passed'])
            self.assertEqual(result['cleanup'], {'vm_closed': True, 'temporary_state_removed': True})
            self.assertNotIn(PRIVATE, stdout.getvalue() + stderr.getvalue())
            self.assertFalse(any('journalctl' in command or 'sway-fixture.log' in command
                                 for command in peers[0].requests))
            self.assertTrue(all(PRIVATE not in path.read_text() for path in output.iterdir()))

    def test_browser_failure_and_interruption_export_only_typed_failure(self):
        for failure in [RuntimeError(PRIVATE), KeyboardInterrupt(PRIVATE)]:
            with self.subTest(failure=type(failure)), self.flow(failure=failure) as values:
                args, root, control, output, events, stdout, stderr, peers = values
                with self.assertRaises(RuntimeError) as caught:
                    desktop.run_desktop(runner, args)
                self.assertFalse(root.exists())
                self.assertFalse(control.exists())
                self.assertFalse((output / 'result.json').exists())
                report = json.loads((output / 'failure.json').read_text())
                self.assertEqual(set(report), {'version', 'scenario', 'run_id', 'reason', 'stage', 'cleanup'})
                self.assertEqual(report['reason'], 'desktop_setup_or_assertion_failed')
                self.assertEqual(report['stage'], 'phases')
                self.assertNotIn(PRIVATE, stdout.getvalue() + stderr.getvalue() + str(caught.exception) + json.dumps(report))

    def test_close_or_either_removal_failure_prevents_passing_report(self):
        for options in [{'close_error': OSError(PRIVATE)}, {'remove_error': 'owned-vm'}, {'remove_error': 'control'}]:
            with self.subTest(options=options), self.flow(**options) as values:
                args, root, control, output, events, stdout, stderr, peers = values
                with self.assertRaises(RuntimeError) as caught:
                    desktop.run_desktop(runner, args)
                self.assertNotIn('result', events)
                self.assertFalse((output / 'result.json').exists())
                report = json.loads((output / 'failure.json').read_text())
                self.assertEqual(report['reason'], 'owned_cleanup_failed')
                self.assertFalse(report['cleanup']['temporary_state_removed'])
                self.assertNotIn(PRIVATE, stdout.getvalue() + stderr.getvalue() + str(caught.exception) + json.dumps(report))
                self.assertIn('remove-owned-vm', events)
                self.assertIn('remove-control', events)

    def test_unknown_private_phase_fields_are_rejected_after_cleanup(self):
        with self.flow(malformed_phase=True) as values:
            args, root, control, output, events, stdout, stderr, peers = values
            with self.assertRaises(RuntimeError):
                desktop.run_desktop(runner, args)
            self.assertFalse(root.exists())
            self.assertFalse(control.exists())
            self.assertFalse((output / 'result.json').exists())
            self.assertEqual(json.loads((output / 'failure.json').read_text())['reason'], 'desktop_evidence_rejected')
            self.assertNotIn(PRIVATE, ''.join(path.read_text() for path in output.iterdir()) + stdout.getvalue() + stderr.getvalue())


class ScenarioCLI(unittest.TestCase):
    def check(self, evidence, *options):
        with tempfile.TemporaryDirectory(prefix='lab253-cli-test-') as directory:
            source = Path(directory) / 'report.json'
            source.write_text(json.dumps(evidence))
            return subprocess.run([sys.executable, str(Path(runner.__file__)), *options,
                                   '--check-evidence', str(source)], capture_output=True, text=True, timeout=10)

    def test_terminal_remains_default_and_desktop_requires_explicit_scenario(self):
        terminal = terminal_reports.EvidenceCLI().bundle()
        self.assertEqual(self.check(terminal).returncode, 0)
        self.assertEqual(self.check(terminal, '--scenario', 'terminal').returncode, 0)
        self.assertEqual(self.check(terminal, '--scenario', 'desktop').returncode, 1)
        self.assertEqual(self.check(reports.fixture()).returncode, 1)
        desktop_result = self.check(reports.fixture(), '--scenario', 'desktop')
        self.assertEqual(desktop_result.returncode, 0, desktop_result.stderr)
        self.assertEqual(json.loads(desktop_result.stdout)['scenario'], 'desktop')

    def test_unknown_scenario_and_desktop_options_on_terminal_fail_before_vm(self):
        terminal = terminal_reports.EvidenceCLI().bundle()
        self.assertNotEqual(self.check(terminal, '--scenario', 'other').returncode, 0)
        self.assertEqual(self.check(terminal, '--desktop-control', '/tmp/not-created').returncode, 1)
        self.assertEqual(self.check(terminal, '--chrome', '/tmp/not-executed.deb').returncode, 1)


if __name__ == '__main__':
    unittest.main()
