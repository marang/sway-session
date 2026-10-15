"""Strict structural evidence contract for the opt-in desktop KVM scenario.

This checks a report, not the truth of an experiment. Only the owned guest
collector can establish the reported observations. Never include raw diagnostics
in rejection messages: this boundary also protects exported browser evidence.
"""
import json
import re
import uuid

LIMIT = 256 * 1024
ROLES = ('chrome', 'slack')
IDENTITY = {'protocol', 'app_id', 'x11_class', 'x11_instance', 'sandbox_app_id',
            'sandbox_engine', 'sandbox_instance_present'}
HEALTH = {'daemon_binary_current', 'delay_inhibitor', 'daemon_pid', 'doctor', 'inhibitor', 'startup'}
STARTUP = 'getty@tty1/PAM/logind -> Bash -> headless Sway -> candidate daemon + one-shot restore'
PHASES = ('stock.chrome', 'protocol.chrome.wayland', 'protocol.chrome.xwayland',
          'protocol.slack.wayland', 'protocol.slack.xwayland',
          'gui.chrome.cancel', 'gui.chrome.approve', 'gui.slack.cancel', 'gui.slack.approve',
          'trust.chrome.reapprove', 'ambiguity.chrome', 'ambiguity.slack',
          'picker.chrome', 'picker.slack', 'follow.chrome', 'follow.slack', 'pinned.chrome', 'pinned.slack')
PACKAGES = {'sway', 'alacritty', 'mesa', 'ttf-dejavu', 'python', 'jq', 'sqlite', 'dbus',
            'openssh', 'polkit', 'flatpak', 'bubblewrap', 'xorg-xwayland', 'grim',
            'wtype', 'noto-fonts', 'xdg-desktop-portal', 'xdg-desktop-portal-wlr'}
SLACK_REF = 'app/com.slack.Slack/x86_64/stable'
CLASSIFICATION = 'fixed-normal-launchers-v1'
SOURCE_PINS = {
    'version': 1,
    'slack_commit': 'a12a31786186231c99d2b4703f9017be95cf18e7eb452d7ab5b338f272b26f25',
    'manifest_sha256': '2f06951f787f2a5e7bf0cf13c1aa6fc5b9bc267921ab89815069b9f64f341ee8',
    'entry_sha256': 'cf864104da3c4280541332fa8da8859ed6113055b22aaad3d4c68843ff648602',
    'wrapper_sha256': '766e0a6f5eedc28c66653d65c1de4aa44af6b8e083ee25ea639e90648e8163dd',
    'helper_sha256': 'dbe941f4baf7fbc35c891048414e1b9468d030e035556f1bdba2eeef96bfaf20',
    'zypak_source_commit': '693a71c5ffa80ec9c9ce2ae03b1ccc493c698e53',
}
SOURCE_FIELDS = set(SOURCE_PINS) | {'portal_sha256'}
# Chrome exposes the controlled empty guest profile in its X11 instance. Only
# this fixture-owned literal is allowed in addition to ordinary identity tokens.
CHROME_X11_INSTANCE = 'google-chrome (/home/ss-test/.config/lab253-chrome)'
OBSERVATION = {'version', 'scenario', 'workspace', 'contexts', 'applications', 'live', 'stored',
               'application_count', 'staging_managed_windows', 'matches_fixture', 'exclusions',
               'run_id', 'boot_id', 'artifact', 'versions', 'launch_trace', 'stable_samples'} | HEALTH


def _require(ok, code='invalid value'):
    if not ok:
        raise RuntimeError('desktop evidence: ' + code)


def _keys(value, required, optional=()):
    _require(type(value) is dict and set(required) <= value.keys()
             and value.keys() <= set(required) | set(optional), 'invalid object keys')


def _eq(value, expected):
    _require(type(value) is type(expected))
    if type(expected) is dict:
        _require(value.keys() == expected.keys(), 'invalid object keys')
        for key in expected:
            _eq(value[key], expected[key])
    elif type(expected) is list:
        _require(len(value) == len(expected), 'invalid list')
        for actual, wanted in zip(value, expected):
            _eq(actual, wanted)
    else:
        _require(value == expected)


def _number(value, low=0, high=1000000):
    _require(type(value) is int and low <= value <= high, 'invalid count')


def _token(value, empty=False):
    _require(type(value) is str and (empty and value == '' or
             re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:@+~/-]{0,127}', value) is not None),
             'invalid public token')


def _sha(value):
    _require(type(value) is str and re.fullmatch(r'[0-9a-f]{64}', value) is not None, 'invalid digest')


def _uuid(value, v4=False):
    _require(type(value) is str, 'invalid UUID')
    try:
        parsed = uuid.UUID(value)
    except (ValueError, AttributeError):
        raise RuntimeError('desktop evidence: invalid UUID') from None
    _require(str(parsed) == value and (not v4 or parsed.version == 4), 'invalid UUID')


def _bounded(value):
    # Apply before recursive/schema operations, including callers passing Python
    # objects directly rather than JSON. Cycles exhaust this fixed node budget.
    pending, visited = [(value, 0)], 0
    while pending:
        item, depth = pending.pop()
        visited += 1
        _require(visited <= 12000 and depth <= 18, 'evidence exceeds structural budget')
        if type(item) is dict:
            _require(len(item) <= 64 and all(type(k) is str and len(k) <= 64 for k in item),
                     'evidence exceeds object budget')
            pending.extend((child, depth + 1) for child in item.values())
        elif type(item) is list:
            _require(len(item) <= 128, 'evidence exceeds list budget')
            pending.extend((child, depth + 1) for child in item)
        elif type(item) is str:
            _require(len(item) <= 512 and all(32 <= ord(c) <= 126 for c in item), 'invalid evidence string')
        else:
            _require(item is None or type(item) is bool or
                     type(item) is int and -2147483648 <= item <= 2147483647, 'invalid evidence scalar')
    _require(len(json.dumps(value, separators=(',', ':'), allow_nan=False).encode()) <= LIMIT,
             'evidence exceeds byte budget')


def _artifact(value):
    _keys(value, {'sha256', 'build'})
    _sha(value['sha256'])
    build = value['build']
    _keys(build, {'product_version', 'commit', 'modified'})
    _token(build['product_version'])
    _require(type(build['commit']) is str and re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', build['commit']),
             'invalid candidate commit')
    _require(type(build['modified']) is bool, 'invalid candidate build')


def _assets(value):
    _keys(value, set(ROLES))
    chrome, slack = value['chrome'], value['slack']
    _keys(chrome, {'version', 'source', 'sha256'})
    _token(chrome['version'])
    _eq(chrome['source'], 'https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb')
    _sha(chrome['sha256'])
    _keys(slack, {'version', 'source', 'origin', 'ref', 'commit'})
    _token(slack['version'])
    _eq(slack['origin'], 'flathub')
    _eq(slack['source'], 'https://dl.flathub.org/repo/')
    _eq(slack['ref'], SLACK_REF)
    _sha(slack['commit'])


def _versions(value):
    _keys(value, {'kernel', 'packages', 'systemd', 'herdr', 'observer_sha256',
                  'chrome', 'slack', 'flatpak', 'slack_ref', 'slack_commit'})
    for name in ('kernel', 'herdr', 'chrome', 'slack', 'flatpak'):
        _token(value[name])
    _require(type(value['systemd']) is str and
             re.fullmatch(r'systemd [0-9][A-Za-z0-9 .()+~_-]{0,126}', value['systemd']), 'invalid systemd version')
    _sha(value['observer_sha256'])
    _sha(value['slack_commit'])
    _eq(value['slack_ref'], SLACK_REF)
    packages = value['packages']
    _keys(packages, {'sway', 'python', 'dbus', 'openssh', 'polkit', 'flatpak'}, PACKAGES)
    for version in packages.values():
        _token(version)


def _identity(value, role, protocol='wayland', unsupported=False):
    _keys(value, IDENTITY)
    _eq(value['protocol'], protocol)
    for name in IDENTITY - {'sandbox_instance_present'}:
        if (role == 'chrome' and protocol == 'xwayland' and name == 'x11_instance'
                and value[name] == CHROME_X11_INSTANCE):
            continue
        _token(value[name], empty=True)
    _require(type(value['sandbox_instance_present']) is bool, 'invalid sandbox instance evidence')
    if protocol == 'wayland':
        _require(bool(value['app_id']), 'missing Wayland identity')
        _eq(value['x11_class'], '')
        _eq(value['x11_instance'], '')
    else:
        _require(bool(value['x11_class']) and bool(value['x11_instance']), 'missing XWayland identity')
        _eq(value['app_id'], '')
    if role == 'slack' and not unsupported:
        _eq(value['sandbox_app_id'], 'com.slack.Slack')
        _eq(value['sandbox_engine'], 'org.flatpak')
        _eq(value['sandbox_instance_present'], True)
    elif unsupported:
        _eq(value['sandbox_app_id'], '')
    else:
        for name in ('sandbox_app_id', 'sandbox_engine'):
            _eq(value[name], '')
        _eq(value['sandbox_instance_present'], False)


def _sources(value):
    _keys(value, SOURCE_FIELDS)
    for key, expected in SOURCE_PINS.items():
        _eq(value[key], expected)
    _sha(value['portal_sha256'])


def _trace(value, starts):
    _keys(value, {'complete', 'measurement', 'starts', 'classification', 'helpers', 'unclassified', 'sources'})
    _eq(value['complete'], True)
    _eq(value['measurement'], 'sched_process_exec_fork_exit')
    _eq(value['classification'], CLASSIFICATION)
    _keys(value['helpers'], {'slack'})
    _number(value['helpers']['slack'])
    _eq(value['unclassified'], 0)
    _sources(value['sources'])
    _keys(value['starts'], set(ROLES))
    for role in ROLES:
        _eq(value['starts'][role], starts)


def _health(value):
    _keys(value, HEALTH)
    _eq(value['daemon_binary_current'], True)
    _eq(value['delay_inhibitor'], True)
    _number(value['daemon_pid'], 1, 4194304)
    _eq(value['doctor'], [{'id': 'daemon.lock', 'status': 'ok'}, {'id': 'daemon.binary', 'status': 'ok'}])
    inhibitor = value['inhibitor']
    _keys(inhibitor, {'who', 'uid', 'pid', 'what', 'mode'})
    _eq(inhibitor['who'], 'sway-session')
    _number(inhibitor['uid'], 1, 2147483647)
    _eq(inhibitor['pid'], value['daemon_pid'])
    _eq(inhibitor['what'], 'shutdown:sleep')
    _eq(inhibitor['mode'], 'delay')
    _eq(value['startup'], {'tty': 'tty1', 'service': 'login', 'state': 'active', 'method': STARTUP,
                          'sway_exec': ['sway-session daemon', 'sway-session restore']})


def _layout(value, ids):
    # Exact canonical semantic snapshot: excludes arbitrary tree/content fields,
    # extra singleton wrappers, fullscreen, floating and additional workspaces.
    _eq(value, {'workspaces': [{'name': '98', 'restore_mode': 'layout',
                              'tiling': {'layout': 'tabbed', 'children': [
                                  {'context_id': ids[0]}, {'context_id': ids[1]}]}, 'floating': []}]})


def _baseline(value):
    _keys(value, OBSERVATION)
    _eq(value['version'], 1)
    _eq(value['scenario'], 'desktop')
    _eq(value['workspace'], '98')
    _eq(value['application_count'], 2)
    _eq(value['staging_managed_windows'], 0)
    _eq(value['matches_fixture'], True)
    _eq(value['exclusions'], ['proportions', 'geometry', 'focus'])
    _require(type(value['applications']) is list and len(value['applications']) == 2, 'expected two applications')
    ids = []
    for app, role in zip(value['applications'], ROLES):
        _keys(app, IDENTITY | {'role', 'context_id', 'desired_open', 'policy', 'presence', 'window_count', 'anchor_marked'})
        _eq(app['role'], role)
        _uuid(app['context_id'])
        ids.append(app['context_id'])
        _eq(app['desired_open'], True)
        _eq(app['policy'], 'follow')
        _eq(app['presence'], 'present')
        _eq(app['window_count'], 1)
        _eq(app['anchor_marked'], True)
        _identity({key: app[key] for key in IDENTITY}, role)
    _require(len(set(ids)) == 2, 'application identities are not distinct')
    _eq(value['contexts'], [{'id': context_id, 'state': 'active'} for context_id in sorted(ids)])
    _layout(value['live'], ids)
    _layout(value['stored'], ids)
    _uuid(value['run_id'], v4=True)
    _uuid(value['boot_id'])
    _artifact(value['artifact'])
    _versions(value['versions'])
    _health({key: value[key] for key in HEALTH})
    _trace(value['launch_trace'], 1)
    _eq(value['launch_trace']['sources']['slack_commit'], value['versions']['slack_commit'])
    _number(value['stable_samples'], 3, 120)
    return ids


def validate_desktop_baseline(baseline):
    """Validate the complete Follow/present original two-application baseline."""
    _bounded(baseline)
    _baseline(baseline)


def validate_desktop_observation(observation, baseline, run_id, artifact):
    """Compare each boot with the original baseline, never a rewritten capture."""
    _bounded([observation, baseline, artifact])
    _baseline(baseline)
    _baseline(observation)
    _uuid(run_id, v4=True)
    _artifact(artifact)
    _eq(observation['run_id'], run_id)
    _eq(baseline['run_id'], run_id)
    _eq(observation['artifact'], artifact)
    _eq(baseline['artifact'], artifact)
    for key in ('contexts', 'applications', 'live', 'stored', 'versions'):
        _eq(observation[key], baseline[key])
    _eq(observation['launch_trace']['sources'], baseline['launch_trace']['sources'])
    _eq(observation['inhibitor']['uid'], baseline['inhibitor']['uid'])


def _gui(facts, role, action, context_id):
    _keys(facts, {'surface', 'surface_mapped', 'visually_inspected', 'input', 'callback_invoked',
                  'registration_before', 'registration_after', 'approval_material_before',
                  'approval_material_after', 'context_id', 'repeat_context_id'})
    _eq(facts['surface'], 'swaynag')
    for key in ('surface_mapped', 'visually_inspected'):
        _eq(facts[key], True)
    _eq(facts['input'], 'sway-ipc-pointer')
    before = ROLES.index(role)
    _eq(facts['registration_before'], before)
    _eq(facts['approval_material_before'], before)
    _eq(facts['callback_invoked'], action == 'approve')
    _eq(facts['registration_after'], before + (action == 'approve'))
    _eq(facts['approval_material_after'], facts['approval_material_before'] + (action == 'approve' and role == 'chrome'))
    for key in ('context_id', 'repeat_context_id'):
        _eq(facts[key], context_id if action == 'approve' else '')


def _phase(phase_id, value, ids, assets):
    _keys(value, {'status', 'reason', 'facts'})
    status, facts = value['status'], value['facts']
    family, role, *mode = phase_id.split('.')
    if status == 'not_exposed':
        _require(family == 'picker' or phase_id == 'ambiguity.slack', 'invalid non-exposure claim')
        _eq(value['reason'], 'empty_profile_ui_absent')
        exposed_key = 'picker_exposed' if family == 'picker' else 'second_window_exposed'
        _eq(facts, {'ui_inspected': True, 'empty_profile': True, exposed_key: False, 'automation_available': True})
        return
    unsupported = status == 'unsupported'
    if unsupported:
        _eq(phase_id, 'protocol.slack.xwayland')
        _eq(value['reason'], 'missing_sandbox_identity')
    else:
        _eq(status, 'passed')
        _eq(value['reason'], 'verified')
    if family == 'stock':
        _keys(facts, {'version', 'source', 'startup_wmclass_count', 'stock_catalog_accepted',
                      'stock_registration_accepted', 'malformed_fixture_duplicate_startup_wmclass_count',
                      'malformed_fixture_catalog_rejected', 'malformed_fixture_registration_rejected',
                      'malformed_fixture_rejection', 'fixture_catalog_origin', 'fixture_launcher_kind'})
        _eq(facts['version'], assets['chrome']['version'])
        _eq(facts['source'], 'vendor_debian_package')
        _eq(facts['startup_wmclass_count'], 1)
        _eq(facts['stock_catalog_accepted'], True)
        _eq(facts['stock_registration_accepted'], True)
        _eq(facts['malformed_fixture_duplicate_startup_wmclass_count'], 2)
        _eq(facts['malformed_fixture_catalog_rejected'], True)
        _eq(facts['malformed_fixture_registration_rejected'], True)
        _eq(facts['malformed_fixture_rejection'], 'duplicate_recognized_key')
        _eq(facts['fixture_catalog_origin'], 'user_local')
        _eq(facts['fixture_launcher_kind'], 'desktop_entry')
    elif family == 'protocol':
        _keys(facts, {'identity', 'mapped', 'registration_accepted'})
        _identity(facts['identity'], role, mode[0], unsupported)
        _eq(facts['mapped'], True)
        _eq(facts['registration_accepted'], not unsupported)
    elif family == 'gui':
        _gui(facts, role, mode[0], ids[ROLES.index(role)])
    elif family == 'trust':
        true_keys = {'material_changed', 'reapproval_required', 'trace_complete', 'cancel_surface_mapped',
                     'cancel_visually_inspected', 'cancel_pointer_input', 'cancel_preserved_digest',
                     'approve_surface_mapped', 'approve_visually_inspected', 'approve_pointer_input',
                     'candidate_callback', 'launcher_valid_after'}
        _keys(facts, true_keys | {'blocked_launch_starts', 'launch_starts_after', 'approval_digest_before', 'approval_digest_after'})
        for key in true_keys:
            _eq(facts[key], True)
        _eq(facts['blocked_launch_starts'], 0)
        _eq(facts['launch_starts_after'], 1)
        _sha(facts['approval_digest_before'])
        _sha(facts['approval_digest_after'])
        _require(facts['approval_digest_before'] != facts['approval_digest_after'], 'approval digest did not change')
    elif family == 'ambiguity':
        _keys(facts, {'ui_inspected', 'window_count', 'presence', 'anchor_marked', 'known_fixture_mark_removed', 'restore_mode', 'launch_trace'})
        for key in ('ui_inspected', 'known_fixture_mark_removed'):
            _eq(facts[key], True)
        _eq(facts['window_count'], 2)
        _eq(facts['presence'], 'ambiguous')
        _eq(facts['anchor_marked'], False)
        _eq(facts['restore_mode'], 'placement_only')
        _trace(facts['launch_trace'], 0)
    elif family == 'picker':
        _keys(facts, {'ui_inspected', 'empty_profile', 'picker_exposed', 'observed_presence_sequence',
                      'observed_window_counts', 'observed_total_windows', 'main_anchor_preserved',
                      'main_window_gap_observed', 'desired_open_preserved', 'samples', 'launch_trace'})
        for key in ('ui_inspected', 'empty_profile', 'picker_exposed', 'main_anchor_preserved', 'desired_open_preserved'):
            _eq(facts[key], True)
        _eq(facts['observed_presence_sequence'], ['present'])
        _eq(facts['observed_window_counts'], [1, 2, 1])
        _eq(facts['observed_total_windows'], [2, 3, 2])
        _eq(facts['main_window_gap_observed'], False)
        _number(facts['samples'], 3, 4096)
        _trace(facts['launch_trace'], 0)
    else:
        common = {'context_id', 'context_state', 'policy', 'present_before', 'fresh_close_event',
                  'production_close_grace_ms', 'steady_samples', 'desired_open', 'launch_trace'}
        if family == 'follow':
            _keys(facts, common | {'absent_after', 'absence_ms', 'health'})
            _eq(facts['absent_after'], True)
            _number(facts['absence_ms'], 2001, 120000)
            _health(facts['health'])
        else:
            _keys(facts, common | {'daemon_restart', 'sway_reload', 'compositor_before', 'compositor_after',
                                  'absent_after_restart', 'absent_after_reload', 'restart_absence_ms', 'reload_absence_ms'})
            for key in ('daemon_restart', 'sway_reload', 'absent_after_restart', 'absent_after_reload'):
                _eq(facts[key], True)
            _number(facts['compositor_before'], 1, 4194304)
            _eq(facts['compositor_after'], facts['compositor_before'])
            for key in ('restart_absence_ms', 'reload_absence_ms'):
                _number(facts[key], 2001, 120000)
        _eq(facts['context_id'], ids[ROLES.index(role)])
        _eq(facts['context_state'], 'active')
        _eq(facts['policy'], family)
        _eq(facts['desired_open'], family == 'pinned')
        _eq(facts['production_close_grace_ms'], 2000)
        _number(facts['steady_samples'], 3, 120)
        for key in ('present_before', 'fresh_close_event'):
            _eq(facts[key], True)
        _trace(facts['launch_trace'], 0)


def validate_desktop_evidence(evidence):
    """Reject incomplete/private/degraded reports; return fixed structural checks."""
    _bounded(evidence)
    _keys(evidence, {'version', 'scenario', 'source', 'run_id', 'artifact', 'base_image_sha256',
                     'herdr_sha256', 'qemu_version', 'app_assets', 'phases', 'observations',
                     'reboot_requests', 'cleanup'}, {'checks'})
    _eq(evidence['version'], 1)
    _eq(evidence['scenario'], 'desktop')
    _eq(evidence['source'], 'qemu-kvm')
    _uuid(evidence['run_id'], v4=True)
    _artifact(evidence['artifact'])
    _sha(evidence['base_image_sha256'])
    _sha(evidence['herdr_sha256'])
    _require(type(evidence['qemu_version']) is str and re.fullmatch(
        r'QEMU emulator version [0-9][A-Za-z0-9 .()+~_-]{0,126}', evidence['qemu_version']), 'invalid QEMU version')
    _assets(evidence['app_assets'])
    observations = evidence['observations']
    _require(type(observations) is list and len(observations) == 3, 'expected baseline and two boots')
    baseline = observations[0]
    ids = _baseline(baseline)
    for observation in observations:
        validate_desktop_observation(observation, baseline, evidence['run_id'], evidence['artifact'])
        versions = observation['versions']
        _eq(versions['chrome'], evidence['app_assets']['chrome']['version'])
        _eq(versions['slack'], evidence['app_assets']['slack']['version'])
        _eq(versions['slack_ref'], evidence['app_assets']['slack']['ref'])
        _eq(versions['slack_commit'], evidence['app_assets']['slack']['commit'])
    boots = [observation['boot_id'] for observation in observations]
    _require(len(set(boots)) == 3, 'expected three distinct boot UUIDs')
    requests = evidence['reboot_requests']
    _require(type(requests) is list and len(requests) == 2, 'expected two regular reboot requests')
    for index, request in enumerate(requests):
        _eq(request, {'accepted': True, 'method': 'guest systemd timer -> systemctl reboot',
                      'before_boot_id': boots[index], 'after_boot_id': boots[index + 1]})
    _keys(evidence['phases'], set(PHASES))
    for phase_id in PHASES:
        _phase(phase_id, evidence['phases'][phase_id], ids, evidence['app_assets'])
        trace = evidence['phases'][phase_id]['facts'].get('launch_trace')
        if trace is not None:
            _eq(trace['sources'], baseline['launch_trace']['sources'])
    # Bind native protocol probes to the original authoritative registered identities.
    for role, app in zip(ROLES, baseline['applications']):
        _eq(evidence['phases']['protocol.' + role + '.wayland']['facts']['identity'],
            {key: app[key] for key in IDENTITY})
        _eq(evidence['phases']['follow.' + role]['facts']['health']['inhibitor']['uid'], baseline['inhibitor']['uid'])
    _eq(evidence['cleanup'], {'vm_closed': True, 'temporary_state_removed': True})
    checks = {'passed': True, 'scenario': 'desktop', 'guest_reboots': 2, 'target_workspace': '98',
              'managed_windows': 2, 'phase_count': 18, 'production_notebook_tested': False}
    if 'checks' in evidence:
        _eq(evidence['checks'], checks)
    return checks
