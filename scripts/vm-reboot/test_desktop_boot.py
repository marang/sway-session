"""Synthetic collection boundaries; these tests never launch apps or a VM."""
import copy
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

import desktop_boot as boot
from test_desktop_evidence import sources as source_proof, trace as trace_report

RUN = 'b3dc5c84-4607-49f3-b65d-76e7682f1d0a'
BOOT = 'fa074a61-5fb7-4899-9739-f93df26a5ff7'
COLD_BOOT = '41363ed9-35cd-47b6-b6a3-375b0fc90736'
IDS = ['0bc6a128-e421-4f86-90b9-e42c187b143a', 'ef4b0a27-7a43-477e-9721-823c86e090a5']
BARRIER = '417ba301-b410-4451-9c24-8abc7a36ab73'
ARTIFACT = {'sha256': 'a' * 64, 'build': {'product_version': '0.1.0-dev', 'commit': 'b' * 40, 'modified': False}}


def snapshot(starts=5, *, boot_id=BOOT, events=100):
    return {'boot_id': boot_id, 'starts': {'chrome': starts, 'slack': starts},
            'callbacks': 3, 'events': events, 'barrier': BARRIER,
            'measurement': 'sched_process_exec_fork_exit', 'classification': 'fixed-normal-launchers-v1',
            'helpers': {'slack': starts}, 'unclassified': 0, 'sources': source_proof()}


def scope():
    return {'version': 1, 'run_id': RUN, 'boot_id': BOOT, 'expected_ids': IDS.copy(), 'snapshot': snapshot()}


def harness(started=True):
    return {'version': 1, 'run_id': RUN, 'artifact': copy.deepcopy(ARTIFACT),
            'baseline_started': started, 'expected_ids': IDS.copy() if started else []}


def report():
    layout = {'workspaces': [{'name': '98', 'restore_mode': 'layout', 'tiling': {
        'layout': 'tabbed', 'children': [{'context_id': value} for value in IDS]}, 'floating': []}]}
    applications = []
    for index, role in enumerate(['chrome', 'slack']):
        applications.append({'role': role, 'context_id': IDS[index], 'desired_open': True, 'policy': 'follow',
            'protocol': 'wayland', 'app_id': 'google-chrome' if index == 0 else 'com.slack.Slack',
            'x11_class': '', 'x11_instance': '', 'sandbox_app_id': '' if index == 0 else 'com.slack.Slack',
            'sandbox_engine': '' if index == 0 else 'org.flatpak', 'sandbox_instance_present': index == 1,
            'presence': 'present', 'window_count': 1, 'anchor_marked': True})
    return {'version': 1, 'scenario': 'desktop', 'workspace': '98',
        'contexts': [{'id': value, 'state': 'active'} for value in IDS],
        'applications': applications, 'live': layout, 'stored': copy.deepcopy(layout),
        'application_count': 2, 'staging_managed_windows': 0, 'matches_fixture': True,
        'exclusions': ['proportions', 'geometry', 'focus'], 'run_id': RUN, 'boot_id': BOOT,
        'artifact': copy.deepcopy(ARTIFACT), 'stable_samples': 3,
        'launch_trace': trace_report(1),
        'versions': {'kernel': '6.17.1-arch1-1', 'packages': {'sway': '1.12-1', 'python': '3.14.7-1',
            'dbus': '1.16.2-1', 'openssh': '10.6p1-1', 'polkit': '127-1', 'flatpak': '1.18.4-1'},
            'systemd': 'systemd 262 (262-1-arch)', 'herdr': '0.9.2', 'observer_sha256': 'c' * 64,
            'chrome': '155.0.8059.79', 'slack': '4.52.178', 'flatpak': '1.18.4',
            'slack_ref': 'app/com.slack.Slack/x86_64/stable', 'slack_commit': source_proof()['slack_commit']},
        'daemon_binary_current': True, 'delay_inhibitor': True, 'daemon_pid': 700,
        'doctor': [{'id': 'daemon.lock', 'status': 'ok'}, {'id': 'daemon.binary', 'status': 'ok'}],
        'inhibitor': {'who': 'sway-session', 'uid': 1000, 'pid': 700, 'what': 'shutdown:sleep', 'mode': 'delay'},
        'startup': {'tty': 'tty1', 'service': 'login', 'state': 'active',
            'method': 'getty@tty1/PAM/logind -> Bash -> headless Sway -> candidate daemon + one-shot restore',
            'sway_exec': ['sway-session daemon', 'sway-session restore']}}


def absent_probe():
    return {'version': 1, 'scenario': 'desktop_probe', 'total_windows': 0, 'focused': None,
        'capture_modes': [], 'groups': [{'context_id': value, 'state': 'active', 'desired_open': True,
            'policy': 'pinned', 'presence': 'absent', 'window_count': 0, 'anchor_marked': False,
            'anchor': None, 'windows': []} for value in IDS]}


def inventory():
    return {'applications': [
        {'context_id': IDS[1], 'state': 'active', 'launcher': {'kind': 'flatpak', 'flatpak_id': 'com.slack.Slack', 'installation': 'user'}},
        {'context_id': IDS[0], 'state': 'active', 'launcher': {'kind': 'desktop', 'desktop_id': 'lab253-chrome.desktop'}}]}


class DesktopBootTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.account = SimpleNamespace(pw_uid=1000, pw_name='ss-test', pw_dir='/home/ss-test')
        self.socket = Path('/run/user/1000/sway-ipc.1000.1.sock')
        self.patch(boot, 'SCOPE', Path(self.temp.name) / 'scope.json')
        self.patch(boot.guest, 'guard', return_value=(RUN, self.account))
        self.patch(boot, 'boot_id', return_value=BOOT)
        self.patch(boot.guest, 'private_artifact')
        self.patch(boot.guest, 'guest_socket', return_value=self.socket)

    def patch(self, target, attribute, *args, **kwargs):
        value = patch.object(target, attribute, *args, **kwargs)
        result = value.start()
        self.addCleanup(value.stop)
        return result

    def store_scope(self, value=None):
        boot.SCOPE.write_text(json.dumps(scope() if value is None else value))
        boot.SCOPE.chmod(0o600)

    def stable(self, values, *, seconds=10):
        self.patch(boot, 'TIMEOUT', seconds)
        self.patch(boot.time, 'monotonic', side_effect=range(100))
        self.sleep = self.patch(boot.time, 'sleep')
        self.samples = self.patch(boot, 'sample', side_effect=values)
        return boot.stable_observation(RUN, self.account, harness(), scope())

    def test_scope_requires_absent_canonical_groups_and_cannot_be_overwritten(self):
        self.patch(boot.guest, 'load_harness', return_value=harness(False))
        self.patch(boot.desktop_guest, 'cli', return_value=inventory())
        self.patch(boot.desktop_guest, 'probe', return_value=absent_probe())
        self.patch(boot.desktop_trace, 'snapshot', return_value=snapshot())
        self.assertEqual(boot.start_baseline_scope(RUN), {'scope_started': True})
        saved = boot.load_scope(RUN)
        self.assertEqual(saved, scope())
        self.assertEqual(boot.SCOPE.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(RuntimeError):
            boot.start_baseline_scope(RUN)
        self.assertEqual(boot.load_scope(RUN), saved)

    def test_scope_rejects_any_present_window_extra_group_or_wrong_launcher(self):
        self.patch(boot.guest, 'load_harness', return_value=harness(False))
        inv = self.patch(boot.desktop_guest, 'cli', return_value=inventory())
        probe = self.patch(boot.desktop_guest, 'probe', return_value=absent_probe())
        trace = self.patch(boot.desktop_trace, 'snapshot', return_value=snapshot())
        for changed in ['window', 'presence', 'anchor', 'archived', 'wrong ID', 'extra group', 'wrong launcher']:
            with self.subTest(changed=changed):
                value, inv_value = absent_probe(), inventory()
                if changed == 'window': value['total_windows'] = 1
                if changed == 'presence': value['groups'][0]['presence'] = 'present'
                if changed == 'anchor': value['groups'][0]['anchor'] = {'container_id': 99, 'workspace': '98'}
                if changed == 'archived': value['groups'][0]['state'] = 'archived'
                if changed == 'wrong ID': value['groups'][0]['context_id'] = RUN
                if changed == 'extra group': value['groups'].append(value['groups'][0])
                if changed == 'wrong launcher': inv_value['applications'][1]['launcher']['desktop_id'] = 'other.desktop'
                probe.return_value, inv.return_value = value, inv_value
                with self.assertRaises(RuntimeError): boot.start_baseline_scope(RUN)
                self.assertFalse(boot.SCOPE.exists())
        trace.assert_not_called()

    def test_scope_load_rejects_run_permissions_snapshot_and_unknown_content(self):
        for changed in ['run', 'permissions', 'snapshot boot', 'counter', 'unknown content', 'IDs']:
            with self.subTest(changed=changed):
                value = scope()
                if changed == 'run': value['run_id'] = COLD_BOOT
                if changed == 'snapshot boot': value['snapshot']['boot_id'] = COLD_BOOT
                if changed == 'counter': value['snapshot']['starts']['chrome'] = True
                if changed == 'unknown content': value['private title'] = 'PRIVATE'
                if changed == 'IDs': value['expected_ids'] = [IDS[0], IDS[0]]
                self.store_scope(value)
                if changed == 'permissions': boot.SCOPE.chmod(0o644)
                with self.assertRaises(RuntimeError): boot.load_scope(RUN)

    def test_same_boot_uses_immutable_scope_delta_and_cold_boot_uses_actual_totals(self):
        trace = self.patch(boot.desktop_trace, 'snapshot', return_value=snapshot(6, events=200))
        expected = report()['launch_trace']
        self.assertEqual(boot.launch_trace(RUN, scope()), expected)
        self.patch(boot, 'boot_id', return_value=COLD_BOOT)
        trace.return_value = snapshot(1, boot_id=COLD_BOOT)
        self.assertEqual(boot.launch_trace(RUN, scope()), expected)
        trace.return_value = snapshot(2, boot_id=COLD_BOOT)
        self.assertEqual(boot.launch_trace(RUN, scope())['starts'], {'chrome': 2, 'slack': 2})

    def test_same_boot_counter_regression_and_wrong_recorder_boot_fail(self):
        trace = self.patch(boot.desktop_trace, 'snapshot', return_value=snapshot(4))
        with self.assertRaises(RuntimeError): boot.launch_trace(RUN, scope())
        trace.return_value = snapshot(6, events=99)
        with self.assertRaises(RuntimeError): boot.launch_trace(RUN, scope())
        trace.return_value = snapshot(1, boot_id=COLD_BOOT)
        with self.assertRaises(RuntimeError): boot.launch_trace(RUN, scope())

    def test_three_matching_samples_are_required_and_errors_reset_stability(self):
        value = report()
        value.pop('stable_samples')
        changed = copy.deepcopy(value)
        changed['daemon_pid'] = changed['inhibitor']['pid'] = 701
        result = self.stable([value, value, RuntimeError('PRIVATE'), changed, value, value, value])
        self.assertEqual(result, report())
        self.assertEqual(self.samples.call_count, 7)
        self.assertEqual(self.sleep.call_count, 6)

    def test_incomplete_stability_and_bad_metadata_never_become_evidence(self):
        for change in ['unstable', 'trace', 'doctor', 'candidate', 'private', 'version']:
            with self.subTest(change=change):
                value = report()
                value.pop('stable_samples')
                if change == 'trace': value['launch_trace']['starts']['chrome'] = 2
                if change == 'doctor': value['delay_inhibitor'] = False
                if change == 'candidate': value['artifact']['sha256'] = 'PRIVATE'
                if change == 'private': value['title'] = 'PRIVATE'
                if change == 'version': value['versions']['chrome'] = 'PRIVATE VERSION WITH SPACES'
                values = [copy.deepcopy(value) for _ in range(5)]
                if change == 'unstable':
                    for index, item in enumerate(values): item['daemon_pid'] = index + 1
                with self.assertRaises(RuntimeError): self.stable(values, seconds=5)

    def test_sample_joins_exact_candidate_health_versions_and_trace_without_launch(self):
        value = report()
        bare = {key: value[key] for key in value if key not in boot.desktop_evidence.HEALTH and key not in {
            'run_id', 'boot_id', 'artifact', 'stable_samples', 'launch_trace', 'versions'}}
        user = self.patch(boot.desktop_guest, 'user', return_value=json.dumps(bare))
        health = self.patch(boot.guest, 'health', return_value={key: value[key] for key in boot.desktop_evidence.HEALTH})
        artifact = self.patch(boot.guest, 'artifact_identity', return_value=ARTIFACT)
        self.patch(boot.desktop_guest, 'versions', return_value=value['versions'])
        self.patch(boot.desktop_trace, 'snapshot', return_value=snapshot(6, events=200))
        result = boot.sample(RUN, self.account, harness(), scope())
        result['stable_samples'] = 3
        self.assertEqual(result, value)
        health.assert_called_once_with(self.account, self.socket, candidate=Path('/usr/bin/sway-session'))
        artifact.assert_called_once_with(candidate=Path('/usr/bin/sway-session'))
        argv = user.call_args.args[1]
        self.assertEqual(argv[1:3], ['--scenario', 'desktop'])
        self.assertEqual(argv[-2:], ['--expected', ','.join(IDS)])
        artifact.return_value = dict(ARTIFACT, sha256='e' * 64)
        with self.assertRaises(RuntimeError): boot.sample(RUN, self.account, harness(), scope())

    def test_baseline_retry_keeps_ids_and_scope_and_only_sets_tabbed_layout(self):
        self.store_scope()
        state = harness(False)
        self.patch(boot.guest, 'load_harness', return_value=state)
        writer = self.patch(boot.guest, 'write_harness')
        sway = self.patch(boot.guest, 'sway_command')
        collector = self.patch(boot, 'stable_observation', side_effect=[RuntimeError('capture not ready'), report()])
        with self.assertRaises(RuntimeError): boot.baseline(RUN, self.account, IDS)
        self.assertEqual(boot.baseline(RUN, self.account, IDS), report())
        writer.assert_called_once_with(state)
        self.assertEqual(sway.call_count, 2)
        for call in sway.call_args_list:
            self.assertEqual(call.args, (self.account, self.socket, 'workspace 98; layout tabbed'))
        self.assertEqual(state['expected_ids'], IDS)
        self.assertEqual(boot.load_scope(RUN), scope())
        with self.assertRaises(RuntimeError): boot.baseline(RUN, self.account, list(reversed(IDS)))
        self.assertEqual(collector.call_count, 2)

    def test_observe_is_read_only_and_rejects_changed_saved_ids(self):
        self.store_scope()
        state = harness()
        self.patch(boot.guest, 'load_harness', return_value=state)
        writer = self.patch(boot.guest, 'write_harness')
        sway = self.patch(boot.guest, 'sway_command')
        self.patch(boot, 'stable_observation', return_value=report())
        self.assertEqual(boot.observe(RUN, self.account), report())
        writer.assert_not_called()
        sway.assert_not_called()
        state['expected_ids'].reverse()
        with self.assertRaises(RuntimeError): boot.observe(RUN, self.account)

    def test_every_public_entrypoint_guards_before_effects(self):
        self.patch(boot.guest, 'guard', side_effect=RuntimeError('outside guest'))
        loader = self.patch(boot.guest, 'load_harness')
        for operation in [lambda: boot.start_baseline_scope(RUN), lambda: boot.baseline(RUN, self.account, IDS), lambda: boot.observe(RUN, self.account)]:
            with self.assertRaises(RuntimeError): operation()
        loader.assert_not_called()

    def test_baseline_cannot_be_redefined_on_a_later_boot(self):
        self.store_scope()
        self.patch(boot, 'boot_id', return_value=COLD_BOOT)
        writer = self.patch(boot.guest, 'write_harness')
        sway = self.patch(boot.guest, 'sway_command')
        with self.assertRaises(RuntimeError): boot.baseline(RUN, self.account, IDS)
        writer.assert_not_called()
        sway.assert_not_called()

    def test_successful_cli_baseline_passes_explicit_role_order(self):
        operation = self.patch(boot, 'baseline', return_value=report())
        output = io.StringIO()
        with redirect_stdout(output):
            self.assertEqual(boot.main(['baseline', '--expected', ','.join(IDS)]), 0)
        self.assertEqual(json.loads(output.getvalue()), report())
        operation.assert_called_once_with(RUN, self.account, IDS)

    def test_wrong_run_or_account_is_rejected(self):
        with self.assertRaises(RuntimeError): boot.guarded(COLD_BOOT, self.account)
        with self.assertRaises(RuntimeError): boot.guarded(RUN, SimpleNamespace(pw_uid=0))

    def test_cli_exports_only_typed_failure_on_raw_or_argument_errors(self):
        self.patch(boot.guest, 'guard', side_effect=RuntimeError('PRIVATE TITLE URL ENV'))
        for args in [['observe'], ['baseline', '--expected', 'PRIVATE'], ['PRIVATE ARGUMENT']]:
            output = io.StringIO()
            with redirect_stdout(output): self.assertEqual(boot.main(args), 1)
            self.assertEqual(json.loads(output.getvalue()), {'failed': True, 'reason': boot.FAILURE})
            self.assertNotIn('PRIVATE', output.getvalue())


if __name__ == '__main__':
    unittest.main()
