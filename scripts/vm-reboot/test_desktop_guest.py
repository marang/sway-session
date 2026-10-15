import importlib.util
import copy
import sys
from pathlib import Path
import tempfile
import json
import subprocess
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import desktop_guest as desktop
import desktop_trace as trace
import desktop_phases as phases


def recorder():
    return trace.Incarnations(sources={**trace.SOURCE_PINS, 'portal_sha256': 'a' * 64})


def fork(parent, child):
    return f'private-1 [000] .... 1.0: sched_process_fork: comm=private pid={parent} child_comm=private child_pid={child}'


def execute(pid, path, old=None):
    return f'private-1 [000] .... 1.1: sched_process_exec: filename={path} pid={pid} old_pid={pid if old is None else old}'


def exit_process(pid):
    return f'private-1 [000] .... 1.2: sched_process_exit: comm=private pid={pid} prio=120'


class ProcessTraceTests(unittest.TestCase):
    def test_each_kernel_loss_counter_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            stats = root / 'per_cpu/cpu0/stats'
            stats.parent.mkdir(parents=True)
            for selected in ['overrun', 'commit overrun', 'dropped events']:
                stats.write_text('\n'.join(f'{key}: {int(key == selected)}' for key in
                                           ['overrun', 'commit overrun', 'dropped events']))
                with self.subTest(selected=selected), self.assertRaises(RuntimeError):
                    trace.overrun(root)

    def test_independent_short_root_counts_while_helpers_and_reexec_do_not(self):
        record = recorder()
        for line in [fork(1, 20), execute(20, '/opt/google/chrome/chrome'),
                     fork(20, 21), execute(21, '/opt/google/chrome/chrome'),
                     execute(20, '/opt/google/chrome/chrome'),
                     fork(1, 30), execute(30, '/opt/google/chrome/chrome'), exit_process(30)]:
            record.consume(line)
        self.assertEqual(record.starts, {'chrome': 2, 'slack': 0})
        self.assertNotIn('private', str(record.state('boot')))

    def test_dead_parent_descendant_keeps_incarnation_and_recycled_pid_is_fresh(self):
        record = recorder()
        for line in [fork(1, 20), execute(20, '/app/bin/com.slack.Slack'),
                     execute(20, '/app/bin/zypak-wrapper'), execute(20, '/app/bin/zypak-helper'),
                     execute(20, '/app/extra/slack'), fork(20, 21),
                     exit_process(20), execute(21, '/app/extra/slack'),
                     fork(1, 20), execute(20, '/app/bin/com.slack.Slack'),
                     execute(20, '/app/bin/zypak-wrapper'), execute(20, '/app/bin/zypak-helper'),
                     execute(20, '/app/extra/slack')]:
            record.consume(line)
        self.assertEqual(record.starts['slack'], 2)

    def test_exec_thread_leader_remapping_does_not_double_count(self):
        record = recorder()
        for line in [fork(1, 20), execute(20, '/opt/google/chrome/chrome'), fork(20, 21),
                     execute(20, '/opt/google/chrome/chrome', old=21)]:
            record.consume(line)
        self.assertEqual(record.starts['chrome'], 1)
        self.assertNotIn(21, record.processes)

    def test_callback_requires_swaynag_process_ancestry(self):
        record = recorder()
        for line in [fork(1, 10), execute(10, '/usr/bin/sway-session'),
                     fork(1, 20), execute(20, '/usr/bin/swaynag'), fork(20, 21),
                     execute(21, '/bin/sh'), fork(21, 22), execute(22, '/usr/bin/sway-session')]:
            record.consume(line)
        self.assertEqual(record.callbacks, 1)

    def test_unknown_events_and_malformed_exec_fail_closed(self):
        for line in ['task: sched_process_exec: unexpected', 'task: sys_enter_execve: argv=private']:
            with self.subTest(line=line), self.assertRaises(RuntimeError):
                recorder().consume(line)

    def test_trace_scope_cannot_cross_boot_or_regress(self):
        before = recorder().state('a')
        before.update(events=10, starts={'chrome': 1, 'slack': 0})
        after = copy.deepcopy(before)
        after.update(boot_id='b', events=20)
        with self.assertRaises(RuntimeError):
            trace.delta(before, after)
        after['boot_id'], after['starts']['chrome'] = 'a', 0
        with self.assertRaises(RuntimeError):
            trace.delta(before, after)


class GUIControllerTests(unittest.TestCase):
    def test_picker_completion_cannot_claim_exposure_without_actual_extra_window(self):
        samples = [{'presence': 'present', 'window_count': 1, 'total_windows': 2,
                    'desired_open': True, 'anchor_preserved': True}] * 3
        state = {'capability': {'inspections': 2, 'origin': {}, 'samples': samples}}
        with patch.object(phases, 'capability_samples'), patch.object(phases.trace, 'snapshot', return_value={}), \
             patch.object(phases.trace, 'delta', return_value={'starts': {'chrome': 0, 'slack': 0}}), \
             self.assertRaises(RuntimeError):
            phases.respond_capability(state, 'run', None, None, 'picker.chrome', 'approve', None, None)

    def test_picker_completion_rejects_lost_known_anchor_or_intent(self):
        for field in ['anchor_preserved', 'desired_open']:
            samples = [{'presence': 'present', 'window_count': count, 'total_windows': count + 1,
                        'desired_open': True, 'anchor_preserved': True} for count in [1, 2, 1]]
            samples[1][field] = False
            state = {'capability': {'inspections': 2, 'origin': {}, 'samples': samples}}
            with self.subTest(field=field), patch.object(phases, 'capability_samples'), \
                 patch.object(phases.trace, 'snapshot', return_value={}), \
                 patch.object(phases.trace, 'delta', return_value={'starts': {'chrome': 0, 'slack': 0}}), \
                 self.assertRaises(RuntimeError):
                phases.respond_capability(state, 'run', None, None, 'picker.chrome', 'approve', None, None)

    def test_version_observation_never_executes_chrome_inside_trace_scope(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'desktop-chrome-version').write_text('155.0.8059.79\n')
            def user(_account, argv, _socket):
                if argv[:3] == ['flatpak', 'list', '--user']:
                    return 'com.slack.Slack\t4.52.178\n'
                return 'app/com.slack.Slack/x86_64/stable\n' if '--show-ref' in argv else 'a' * 64
            def command(argv):
                return 'Flatpak 1.18.4\n' if argv[0] == 'flatpak' else '\n'.join(name + ' 1.0' for name in argv[2:])
            with patch.object(desktop.guest, 'ROOT', root), patch.object(desktop.guest, 'private_artifact'), \
                 patch.object(desktop.guest, 'versions', return_value={'packages': {}, 'herdr': 'herdr 0.9.2'}), \
                 patch.object(desktop, 'user', side_effect=user) as calls, \
                 patch.object(desktop.guest, 'command', side_effect=command):
                value = desktop.versions(None, None)
            self.assertEqual(value['chrome'], '155.0.8059.79')
            self.assertEqual(value['herdr'], '0.9.2')
            self.assertEqual(value['flatpak'], '1.18.4')
            self.assertTrue(all(call.args[1][0] == 'flatpak' for call in calls.call_args_list))

    def test_pointer_rejects_any_coordinate_outside_owned_fixed_output(self):
        for x, y in [(-1, 0), (1280, 0), (0, 720), (True, 10), (None, 5)]:
            with self.subTest(x=x, y=y), patch.object(desktop.guest, 'sway_command') as command:
                with self.assertRaises(RuntimeError):
                    desktop.click(None, None, x, y)
                command.assert_not_called()

    def test_pointer_routes_real_press_and_release_to_owned_socket(self):
        with patch.object(desktop.guest, 'sway_command') as command:
            desktop.click('owned-account', 'owned-socket', 100, 20)
        self.assertEqual([call.args for call in command.call_args_list], [
            ('owned-account', 'owned-socket', 'seat seat0 cursor set 100 20'),
            ('owned-account', 'owned-socket', 'seat seat0 cursor press button1'),
            ('owned-account', 'owned-socket', 'seat seat0 cursor release button1')])

    def test_registration_action_cannot_claim_uninspected_surface(self):
        with patch.object(desktop, 'probe') as probe, self.assertRaises(RuntimeError):
            desktop.register_action('run', None, None, 'chrome', approve=True, x=10, y=10)
        probe.assert_not_called()


class FailureDiagnosticTests(unittest.TestCase):
    def raised(self, filename, message='PRIVATE-URL-ARGV-ENV-PROFILE'):
        namespace = {}
        exec(compile('def fail(n):\n if n: return fail(n-1)\n raise RuntimeError(' + repr(message) + ')\n',
                     filename, 'exec'), namespace)
        try:
            namespace['fail'](12)
        except RuntimeError as error:
            return error

    def test_private_contents_never_appear_and_only_six_exact_trusted_frames_remain(self):
        error = self.raised('/opt/sway-session-test/desktop_phases.py')
        value = desktop.failure_summary(error)
        self.assertEqual(set(value), {'version', 'kind', 'frames'})
        self.assertEqual(value['version'], 1)
        self.assertEqual(value['kind'], 'RuntimeError')
        self.assertEqual(len(value['frames']), 6)
        self.assertTrue(all(set(frame) == {'module', 'line'} and frame['module'] == 'desktop_phases'
                            and type(frame['line']) is int and 1 <= frame['line'] <= 10000
                            for frame in value['frames']))
        rendered = json.dumps(value)
        self.assertNotIn('PRIVATE', rendered)
        self.assertNotIn('/opt/', rendered)
        self.assertNotIn('fail', rendered)

    def test_basename_and_normalized_alias_cannot_impersonate_installed_source(self):
        for filename in ['/tmp/desktop_phases.py', '/opt/sway-session-test/../sway-session-test/desktop_phases.py',
                         '/opt/sway-session-test/desktop_unknown.py']:
            with self.subTest(filename=filename):
                self.assertEqual(desktop.failure_summary(self.raised(filename))['frames'], [])

    def test_os_subclass_and_timeout_have_fixed_kinds_without_exception_strings(self):
        for error, expected in [(FileNotFoundError('PRIVATE'), 'OSError'),
                                (subprocess.TimeoutExpired(['PRIVATE'], 1, output=b'PRIVATE', stderr=b'PRIVATE'),
                                 'TimeoutExpired')]:
            value = desktop.failure_summary(error)
            self.assertEqual(value['kind'], expected)
            self.assertNotIn('PRIVATE', json.dumps(value))

    def test_redacted_wrappers_keep_only_trusted_inner_source_locations(self):
        inner = self.raised('/opt/sway-session-test/desktop_lifecycle.py')
        try:
            try:
                raise inner
            except RuntimeError:
                raise RuntimeError('PRIVATE wrapped error') from None
        except RuntimeError as outer:
            value = desktop.failure_summary(outer)
        self.assertTrue(value['frames'])
        self.assertTrue(all(frame['module'] == 'desktop_lifecycle' for frame in value['frames']))
        self.assertNotIn('PRIVATE', json.dumps(value))

    def test_private_diagnostic_is_atomic_mode600_and_never_contains_exception_contents(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(desktop, 'failure_root', return_value=root):
                desktop.write_failure(self.raised('/opt/sway-session-test/desktop_guest.py'))
            path = root / 'desktop-failure.json'
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(path.read_text())['kind'], 'RuntimeError')
            self.assertNotIn('PRIVATE', path.read_text())
            self.assertFalse((root / '.desktop-failure.json.new').exists())

    def test_failed_guard_prevents_any_diagnostic_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(desktop, 'GUARD_PASSED', False), patch.object(desktop.guest, 'ROOT', root):
                desktop.write_failure(RuntimeError('PRIVATE'))
            self.assertEqual(list(root.iterdir()), [])

    def test_existing_temporary_symlink_cannot_redirect_diagnostic(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            victim = root / 'victim'
            victim.write_text('original')
            (root / '.desktop-failure.json.new').symlink_to(victim)
            with patch.object(desktop, 'failure_root', return_value=root):
                desktop.write_failure(RuntimeError('PRIVATE'))
            self.assertEqual(victim.read_text(), 'original')
            self.assertFalse((root / 'desktop-failure.json').exists())


if __name__ == '__main__':
    unittest.main()
