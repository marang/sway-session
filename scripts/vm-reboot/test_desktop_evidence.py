"""Desktop report acceptance boundaries; these fixtures do not prove a VM run."""
import copy
import unittest

from desktop_evidence import (validate_desktop_baseline, validate_desktop_evidence,
                              validate_desktop_observation)

CHROME = '0bc6a128-e421-4f86-90b9-e42c187b143a'
SLACK = 'ef4b0a27-7a43-477e-9721-823c86e090a5'
RUN = 'b3dc5c84-4607-49f3-b65d-76e7682f1d0a'
BOOTS = ['fa074a61-5fb7-4899-9739-f93df26a5ff7', '41363ed9-35cd-47b6-b6a3-375b0fc90736',
         '417ba301-b410-4451-9c24-8abc7a36ab73']
ARTIFACT = {'sha256': 'a' * 64, 'build': {'product_version': '0.1.0-dev', 'commit': 'b' * 40, 'modified': False}}


def trace(starts):
    return {'complete': True, 'measurement': 'sched_process_exec_fork_exit',
            'starts': {'chrome': starts, 'slack': starts}}


def health(pid=700):
    return {'daemon_binary_current': True, 'delay_inhibitor': True, 'daemon_pid': pid,
            'doctor': [{'id': 'daemon.lock', 'status': 'ok'}, {'id': 'daemon.binary', 'status': 'ok'}],
            'inhibitor': {'who': 'sway-session', 'uid': 1000, 'pid': pid, 'what': 'shutdown:sleep', 'mode': 'delay'},
            'startup': {'tty': 'tty1', 'service': 'login', 'state': 'active',
                        'method': 'getty@tty1/PAM/logind -> Bash -> headless Sway -> candidate daemon + one-shot restore',
                        'sway_exec': ['sway-session daemon', 'sway-session restore']}}


def identity(role, protocol='wayland'):
    return {'protocol': protocol, 'app_id': ('google-chrome' if role == 'chrome' else 'com.slack.Slack') if protocol == 'wayland' else '',
            'x11_class': ('Google-chrome' if role == 'chrome' else 'com.slack.Slack') if protocol == 'xwayland' else '',
            'x11_instance': ('google-chrome (/home/ss-test/.config/lab253-chrome)' if role == 'chrome' else 'com.slack.slack') if protocol == 'xwayland' else '',
            'sandbox_app_id': 'com.slack.Slack' if role == 'slack' and protocol == 'wayland' else '',
            'sandbox_engine': 'flatpak' if role == 'slack' and protocol == 'wayland' else '',
            'sandbox_instance_present': role == 'slack' and protocol == 'wayland'}


def baseline():
    layout = {'workspaces': [{'name': '98', 'restore_mode': 'layout', 'tiling': {
        'layout': 'tabbed', 'children': [{'context_id': CHROME}, {'context_id': SLACK}]}, 'floating': []}]}
    apps = []
    for role, context_id in [('chrome', CHROME), ('slack', SLACK)]:
        apps.append(dict(identity(role), role=role, context_id=context_id, desired_open=True,
                         policy='follow', presence='present', window_count=1, anchor_marked=True))
    return dict(health(), version=1, scenario='desktop', workspace='98',
                contexts=[{'id': CHROME, 'state': 'active'}, {'id': SLACK, 'state': 'active'}],
                applications=apps, live=copy.deepcopy(layout), stored=copy.deepcopy(layout),
                application_count=2, staging_managed_windows=0, matches_fixture=True,
                exclusions=['proportions', 'geometry', 'focus'], run_id=RUN, boot_id=BOOTS[0],
                artifact=copy.deepcopy(ARTIFACT), stable_samples=3, launch_trace=trace(1),
                versions={'kernel': '6.17.1-arch1-1', 'packages': {'sway': '1.12-1', 'python': '3.14.7-1',
                          'dbus': '1.16.2-1', 'openssh': '10.6p1-1', 'polkit': '127-1', 'flatpak': '1.18.4-1'},
                          'systemd': 'systemd 262 (262-1-arch)', 'herdr': '0.9.2', 'observer_sha256': 'c' * 64,
                          'chrome': '155.0.8059.79', 'slack': '4.52.155', 'flatpak': '1.18.4',
                          'slack_ref': 'app/com.slack.Slack/x86_64/stable', 'slack_commit': 'd' * 64})


def fixture():
    phases = {}

    def phase(name, facts, status='passed', reason='verified'):
        phases[name] = {'status': status, 'reason': reason, 'facts': facts}

    phase('stock.chrome', {'version': '155.0.8059.79', 'source': 'vendor_debian_package',
                          'startup_wmclass_count': 1, 'stock_catalog_accepted': True,
                          'stock_registration_accepted': True,
                          'malformed_fixture_duplicate_startup_wmclass_count': 2,
                          'malformed_fixture_catalog_rejected': True,
                          'malformed_fixture_registration_rejected': True,
                          'malformed_fixture_rejection': 'duplicate_recognized_key',
                          'fixture_catalog_origin': 'user_local', 'fixture_launcher_kind': 'desktop_entry'})
    for role in ['chrome', 'slack']:
        for protocol in ['wayland', 'xwayland']:
            unsupported = role == 'slack' and protocol == 'xwayland'
            phase('protocol.' + role + '.' + protocol,
                  {'identity': identity(role, protocol), 'mapped': True, 'registration_accepted': not unsupported},
                  'unsupported' if unsupported else 'passed', 'missing_sandbox_identity' if unsupported else 'verified')
    for role, context_id, before in [('chrome', CHROME, 0), ('slack', SLACK, 1)]:
        for action in ['cancel', 'approve']:
            approve = action == 'approve'
            phase('gui.' + role + '.' + action, {'surface': 'swaynag', 'surface_mapped': True,
                  'visually_inspected': True, 'input': 'sway-ipc-pointer', 'callback_invoked': approve,
                  'registration_before': before, 'registration_after': before + int(approve),
                  'approval_material_before': before,
                  'approval_material_after': before + int(approve and role == 'chrome'),
                  'context_id': context_id if approve else '', 'repeat_context_id': context_id if approve else ''})
    phase('trust.chrome.reapprove', {'material_changed': True, 'reapproval_required': True,
          'blocked_launch_starts': 0, 'trace_complete': True, 'cancel_surface_mapped': True,
          'cancel_visually_inspected': True, 'cancel_pointer_input': True, 'cancel_preserved_digest': True,
          'approve_surface_mapped': True, 'approve_visually_inspected': True, 'approve_pointer_input': True,
          'candidate_callback': True, 'approval_digest_before': 'e' * 64, 'approval_digest_after': 'f' * 64,
          'launcher_valid_after': True, 'launch_starts_after': 1})
    phase('ambiguity.chrome', {'ui_inspected': True, 'window_count': 2, 'presence': 'ambiguous',
          'anchor_marked': False, 'known_fixture_mark_removed': True, 'restore_mode': 'placement_only', 'launch_trace': trace(0)})
    phase('ambiguity.slack', {'ui_inspected': True, 'empty_profile': True, 'second_window_exposed': False,
                            'automation_available': True}, 'not_exposed', 'empty_profile_ui_absent')
    for role, context_id in [('chrome', CHROME), ('slack', SLACK)]:
        phase('picker.' + role, {'ui_inspected': True, 'empty_profile': True, 'picker_exposed': False,
                               'automation_available': True}, 'not_exposed', 'empty_profile_ui_absent')
        phase('follow.' + role, {'context_id': context_id, 'context_state': 'active', 'policy': 'follow',
              'present_before': True, 'fresh_close_event': True, 'absent_after': True, 'absence_ms': 3001,
              'production_close_grace_ms': 2000, 'steady_samples': 3, 'desired_open': False,
              'health': health(), 'launch_trace': trace(0)})
        phase('pinned.' + role, {'context_id': context_id, 'context_state': 'active', 'policy': 'pinned',
              'present_before': True, 'fresh_close_event': True, 'desired_open': True,
              'daemon_restart': True, 'sway_reload': True, 'compositor_before': 600, 'compositor_after': 600,
              'absent_after_restart': True, 'absent_after_reload': True, 'restart_absence_ms': 3001,
              'reload_absence_ms': 3001, 'production_close_grace_ms': 2000, 'steady_samples': 3,
              'launch_trace': trace(0)})
    observations = []
    for index, boot in enumerate(BOOTS):
        obs = baseline()
        obs['boot_id'] = boot
        obs.update(health(700 + index * 2))
        observations.append(obs)
    return {'version': 1, 'scenario': 'desktop', 'source': 'qemu-kvm', 'run_id': RUN,
            'artifact': copy.deepcopy(ARTIFACT), 'base_image_sha256': '1' * 64, 'herdr_sha256': '2' * 64,
            'qemu_version': 'QEMU emulator version 11.1.2', 'app_assets': {
                'chrome': {'version': '155.0.8059.79', 'source': 'https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb', 'sha256': '3' * 64},
                'slack': {'version': '4.52.155', 'source': 'https://dl.flathub.org/repo/', 'origin': 'flathub', 'ref': 'app/com.slack.Slack/x86_64/stable', 'commit': 'd' * 64}},
            'phases': phases, 'observations': observations,
            'reboot_requests': [{'accepted': True, 'method': 'guest systemd timer -> systemctl reboot',
                                 'before_boot_id': BOOTS[i], 'after_boot_id': BOOTS[i + 1]} for i in range(2)],
            'cleanup': {'vm_closed': True, 'temporary_state_removed': True}}


def replace(value, path, replacement):
    target = value
    for key in path[:-1]:
        target = target[key]
    target[path[-1]] = replacement


class DesktopEvidence(unittest.TestCase):
    def test_complete_report_and_public_helpers(self):
        evidence = fixture()
        checks = validate_desktop_evidence(evidence)
        self.assertTrue(checks['passed'])
        self.assertEqual(checks['guest_reboots'], 2)
        validate_desktop_baseline(evidence['observations'][0])
        validate_desktop_observation(evidence['observations'][1], evidence['observations'][0], RUN, ARTIFACT)
        evidence['checks'] = checks
        self.assertEqual(validate_desktop_evidence(evidence), checks)

    def test_exposed_picker_requires_the_observed_gap_and_preserved_intent(self):
        evidence = fixture()
        evidence['phases']['picker.chrome'] = {'status': 'passed', 'reason': 'verified', 'facts': {
            'ui_inspected': True, 'empty_profile': True, 'picker_exposed': True,
            'presence_sequence': ['present', 'transient', 'absent', 'present'], 'gap_ms': 230,
            'desired_open_preserved': True, 'launch_trace': trace(0)}}
        validate_desktop_evidence(evidence)
        evidence['phases']['picker.chrome']['facts']['desired_open_preserved'] = False
        with self.assertRaises(RuntimeError):
            validate_desktop_evidence(evidence)

    def test_meaningful_negative_mutations(self):
        cases = [
            (('scenario',), 'terminal'), (('version',), 2), (('source',), 'synthetic'),
            (('run_id',), BOOTS[0].upper()), (('base_image_sha256',), 'unknown'),
            (('app_assets', 'chrome', 'source'), 'https://example.com/untrusted.deb'),
            (('app_assets', 'slack', 'origin'), 'untrusted'),
            (('app_assets', 'slack', 'source'), 'https://example.com/repo/'), (('app_assets', 'slack', 'commit'), 'a' * 64),
            (('observations', 1, 'boot_id'), BOOTS[0]),
            (('observations', 1, 'artifact', 'sha256'), '0' * 64),
            (('observations', 1, 'daemon_binary_current'), False),
            (('observations', 1, 'delay_inhibitor'), False),
            (('observations', 1, 'inhibitor', 'pid'), 1),
            (('observations', 1, 'inhibitor', 'uid'), 0),
            (('observations', 1, 'inhibitor', 'what'), 'shutdown'),
            (('observations', 1, 'startup', 'service'), 'sshd'),
            (('observations', 1, 'doctor', 0, 'status'), 'warning'),
            (('observations', 1, 'stable_samples'), 2),
            (('observations', 1, 'staging_managed_windows'), 1),
            (('observations', 1, 'applications', 0, 'role'), 'slack'),
            (('observations', 1, 'applications', 0, 'policy'), 'pinned'),
            (('observations', 1, 'applications', 0, 'desired_open'), False),
            (('observations', 1, 'applications', 0, 'window_count'), 2),
            (('observations', 1, 'applications', 0, 'presence'), 'ambiguous'),
            (('observations', 1, 'applications', 0, 'anchor_marked'), False),
            (('observations', 1, 'applications', 0, 'app_id'), 'sway-session.' + CHROME),
            (('observations', 1, 'applications', 1, 'sandbox_app_id'), ''),
            (('observations', 1, 'applications', 1, 'sandbox_instance_present'), False),
            (('observations', 1, 'versions', 'chrome'), '154.0.0.0'),
            (('observations', 1, 'live', 'workspaces', 0, 'name'), '1'),
            (('observations', 1, 'live', 'workspaces', 0, 'tiling', 'layout'), 'splith'),
            (('observations', 1, 'stored', 'workspaces', 0, 'tiling', 'children', 0, 'context_id'), SLACK),
            (('observations', 1, 'launch_trace', 'complete'), False),
            (('observations', 1, 'launch_trace', 'starts', 'chrome'), 2),
            (('observations', 1, 'launch_trace', 'measurement'), 'durable_guard'),
            (('reboot_requests', 0, 'accepted'), False),
            (('reboot_requests', 0, 'method'), 'qmp system_reset'),
            (('reboot_requests', 1, 'before_boot_id'), BOOTS[0]),
            (('cleanup', 'vm_closed'), False), (('cleanup', 'temporary_state_removed'), False),
            (('phases', 'gui.chrome.cancel', 'facts', 'registration_after'), 1),
            (('phases', 'gui.chrome.cancel', 'facts', 'approval_material_after'), 1),
            (('phases', 'gui.chrome.approve', 'facts', 'callback_invoked'), False),
            (('phases', 'gui.chrome.approve', 'facts', 'input'), 'direct-app-confirm'),
            (('phases', 'gui.chrome.approve', 'facts', 'visually_inspected'), False),
            (('phases', 'gui.chrome.approve', 'facts', 'repeat_context_id'), SLACK),
            (('phases', 'gui.slack.approve', 'facts', 'approval_material_after'), 2),
            (('phases', 'stock.chrome', 'facts', 'startup_wmclass_count'), 2),
            (('phases', 'stock.chrome', 'facts', 'source'), 'host_distro_package'),
            (('phases', 'stock.chrome', 'facts', 'stock_catalog_accepted'), False),
            (('phases', 'stock.chrome', 'facts', 'stock_registration_accepted'), False),
            (('phases', 'stock.chrome', 'facts', 'malformed_fixture_duplicate_startup_wmclass_count'), 1),
            (('phases', 'stock.chrome', 'facts', 'malformed_fixture_catalog_rejected'), False),
            (('phases', 'stock.chrome', 'facts', 'malformed_fixture_registration_rejected'), False),
            (('phases', 'stock.chrome', 'facts', 'malformed_fixture_rejection'), 'unsupported'),
            (('phases', 'protocol.chrome.xwayland', 'facts', 'identity', 'x11_instance'),
             'google-chrome (/home/markus/.config/google-chrome)'),
            (('phases', 'protocol.chrome.xwayland', 'facts', 'identity', 'x11_instance'),
             'google-chrome (/home/ss-test/.config/uncontrolled-profile)'),
            (('phases', 'protocol.slack.xwayland', 'facts', 'identity', 'x11_instance'),
             'google-chrome (/home/ss-test/.config/lab253-chrome)'),
            (('observations', 0, 'applications', 0, 'x11_instance'),
             'google-chrome (/home/ss-test/.config/lab253-chrome)'),
            (('phases', 'trust.chrome.reapprove', 'facts', 'blocked_launch_starts'), 1),
            (('phases', 'trust.chrome.reapprove', 'facts', 'cancel_preserved_digest'), False),
            (('phases', 'trust.chrome.reapprove', 'facts', 'approval_digest_after'), 'e' * 64),
            (('phases', 'ambiguity.chrome', 'facts', 'anchor_marked'), True),
            (('phases', 'ambiguity.chrome', 'facts', 'restore_mode'), 'layout'),
            (('phases', 'ambiguity.chrome', 'facts', 'known_fixture_mark_removed'), False),
            (('phases', 'picker.chrome', 'facts', 'automation_available'), False),
            (('phases', 'picker.chrome', 'reason'), 'backend_missing'),
            (('phases', 'follow.chrome', 'facts', 'desired_open'), True),
            (('phases', 'follow.chrome', 'facts', 'fresh_close_event'), False),
            (('phases', 'follow.chrome', 'facts', 'absence_ms'), 1000),
            (('phases', 'follow.chrome', 'facts', 'production_close_grace_ms'), 10),
            (('phases', 'follow.chrome', 'facts', 'launch_trace', 'starts', 'chrome'), 1),
            (('phases', 'pinned.chrome', 'facts', 'compositor_after'), 601),
            (('phases', 'pinned.chrome', 'facts', 'daemon_restart'), False),
            (('phases', 'pinned.chrome', 'facts', 'reload_absence_ms'), 100),
            (('phases', 'protocol.slack.xwayland', 'facts', 'mapped'), False),
            (('phases', 'protocol.slack.xwayland', 'facts', 'registration_accepted'), True),
            (('phases', 'protocol.slack.xwayland', 'facts', 'identity', 'sandbox_app_id'), 'com.slack.Slack'),
        ]
        for path, replacement in cases:
            with self.subTest(path=path):
                evidence = fixture()
                replace(evidence, path, replacement)
                with self.assertRaises(RuntimeError):
                    validate_desktop_evidence(evidence)

    def test_missing_unknown_private_and_placeholder_data_are_rejected(self):
        for mutate in [
                lambda e: e['phases'].pop('pinned.slack'),
                lambda e: e['phases']['stock.chrome']['facts'].pop('malformed_fixture_registration_rejected'),
                lambda e: e['observations'][0].pop('applications'),
                lambda e: e['observations'][0].update(terminal_count=2),
                lambda e: e.update(error='secret-private-content'),
                lambda e: e['observations'][0]['applications'][0].update(title='secret-private-content'),
                lambda e: e['phases']['picker.chrome']['facts'].update(url='secret-private-content'),
                lambda e: e['phases'].update(extra_phase={'status': 'passed'}),
                lambda e: e['phases']['gui.chrome.approve'].update(status='pending'),
                lambda e: e['phases']['gui.chrome.approve'].update(status='not_exposed'),
                lambda e: e['phases']['protocol.chrome.xwayland'].update(status='unsupported'),
                lambda e: e['phases']['gui.chrome.approve'].update(facts={}),
                lambda e: e['observations'].append(copy.deepcopy(e['observations'][1])),
                lambda e: e['observations'][0]['contexts'].append({'id': CHROME, 'state': 'active'}),
                lambda e: e.update(checks={'passed': True}),
                lambda e: e['observations'][0]['live']['workspaces'][0]['tiling'].update(fullscreen='global'),
        ]:
            evidence = fixture()
            mutate(evidence)
            with self.assertRaises(RuntimeError) as caught:
                validate_desktop_evidence(evidence)
            self.assertNotIn('secret-private-content', str(caught.exception))
            self.assertLess(len(str(caught.exception)), 100)

    def test_scalar_types_and_budgets_are_strict(self):
        for path, replacement in [(('version',), True), (('cleanup', 'vm_closed'), 1),
                                  (('observations', 0, 'contexts', 0, 'state'), None),
                                  (('observations', 0, 'stable_samples'), 3.0),
                                  (('observations', 0, 'versions', 'kernel'), float('nan')),
                                  (('phases', 'gui.chrome.cancel', 'facts', 'registration_before'), False),
                                  (('qemu_version',), 'QEMU emulator version 1\nprivate'),
                                  (('artifact', 'build', 'product_version'), 'a' * 513)]:
            evidence = fixture()
            replace(evidence, path, replacement)
            with self.assertRaises(RuntimeError):
                validate_desktop_evidence(evidence)
        cyclic = {}
        cyclic['cycle'] = cyclic
        with self.assertRaises(RuntimeError):
            validate_desktop_evidence(cyclic)
        with self.assertRaises(RuntimeError):
            validate_desktop_evidence({'a': [0] * 129})

    def test_rewriting_both_postboot_layouts_does_not_replace_original_baseline(self):
        evidence = fixture()
        for obs in evidence['observations'][1:]:
            for key in ['live', 'stored']:
                obs[key]['workspaces'][0]['tiling']['children'].reverse()
        with self.assertRaises(RuntimeError):
            validate_desktop_evidence(evidence)


if __name__ == '__main__':
    unittest.main()
