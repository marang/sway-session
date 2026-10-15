"""Persistent real-UI acceptance phases and explicit visual checkpoints.

Every visual assertion requires a matching host-controller response after an
agent has viewed the private screenshot. It is never inferred from a launch.
"""
import json
import os
import time
import uuid
from pathlib import Path

import guest
import desktop_guest as d
import desktop_trace as trace

STATE = guest.ROOT / 'desktop-phases.json'
ORDER = ['stock.chrome', 'protocol.chrome.wayland', 'protocol.chrome.xwayland',
         'gui.chrome.cancel', 'gui.chrome.approve', 'protocol.slack.xwayland',
         'protocol.slack.wayland', 'gui.slack.cancel', 'gui.slack.approve',
         'trust.chrome.reapprove', 'ambiguity.chrome', 'ambiguity.slack',
         'picker.chrome', 'picker.slack', 'follow.chrome', 'pinned.chrome',
         'follow.slack', 'pinned.slack', 'baseline']


def passed(facts):
    return {'status': 'passed', 'reason': 'verified', 'facts': facts}


def save(state):
    temporary = STATE.with_suffix('.new')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        json.dump(state, output)
    os.replace(temporary, STATE)


def group(account, socket, context_id):
    rows = [row for row in d.probe(account, socket)['groups'] if row['context_id'] == context_id]
    guest.require(len(rows) == 1, 'expected_registered_fixture_group')
    return rows[0]


def anchor(account, socket, context_id):
    value = group(account, socket, context_id)
    guest.require(value['presence'] == 'present' and value['window_count'] == 1
                  and value['anchor_marked'] and value['anchor']['workspace'] == '98', 'expected_unique_fixture_anchor')
    return value['anchor']['container_id']


def focus(account, socket, context_id):
    container = anchor(account, socket, context_id)
    guest.sway_command(account, socket, f'[con_id={container}] focus')
    d.wait_for(lambda: (value := d.probe(account, socket)['focused'])
               and value.get('context_id') == context_id and value['container_id'] == container)
    return container


def close_unregistered(account, socket):
    value = d.probe(account, socket)
    guest.require(value['focused'] is not None and value['focused']['workspace'] == '98'
                  and 'context_id' not in value['focused'], 'expected_owned_unregistered_focus')
    container = value['focused']['container_id']
    guest.sway_command(account, socket, f'[con_id={container}] kill')
    d.wait_for(lambda: d.probe(account, socket)['total_windows'] == len(value['groups']))
    time.sleep(.5)


def bootstrap(account, socket, role, protocol='wayland'):
    if role == 'chrome':
        d.write_user(account, d.ENTRY, d.chrome_entry('x11' if protocol == 'xwayland' else 'wayland'))
        d.launch_entry(account, socket)
    else:
        options = '--nosocket=wayland --socket=x11 ' if protocol == 'xwayland' else ''
        suffix = ' --ozone-platform=x11' if protocol == 'xwayland' else ''
        guest.sway_command(account, socket, 'exec --no-startup-id /usr/bin/flatpak run --user ' + options + 'com.slack.Slack' + suffix)
    def mapped():
        value = d.probe(account, socket)
        window = value['focused']
        return window if window and window['workspace'] == '98' and window['protocol'] == protocol and 'context_id' not in window else None
    return d.wait_for(mapped, seconds=45)


def register_args(role):
    return ['app', 'register-focused', '--desktop-id', d.DESKTOP_ID if role == 'chrome' else 'com.slack.Slack.desktop']


def stock(account, socket):
    if not d.probe(account, socket)['focused']:
        bootstrap(account, socket, 'chrome')
    path = Path('/usr/share/applications/google-chrome.desktop')
    guest.require(path.stat().st_size <= 1024 * 1024, 'stock_entry_budget')
    main, count = False, 0
    for line in path.read_text().splitlines():
        if line.startswith('['):
            main = line == '[Desktop Entry]'
        elif main and line.startswith('StartupWMClass='):
            count += 1
    status, reply = d.cli_attempt(account, socket, 'app', 'register-focused', '--desktop-id', 'google-chrome.desktop', '--yes')
    codes = {row['code'] for row in reply.get('diagnostics', [])}
    accepted = status == 0
    if accepted:
        guest.require(count == 1 and len(reply.get('contexts', [])) == 1, 'stock_registration_shape')
        d.cli(account, socket, 'app', 'forget', '--yes', reply['contexts'][0]['id'])
    else:
        guest.require(count >= 2 and 'app_ambiguous' in codes, 'unexpected_stock_rejection')
    malformed = d.ENTRY.with_name('lab253-malformed-chrome.desktop')
    d.write_user(account, malformed, d.chrome_entry() + 'StartupWMClass=google-chrome\n')
    try:
        malformed_status, malformed_reply = d.cli_attempt(account, socket, 'app', 'register-focused',
                                                          '--desktop-id', malformed.name, '--yes')
        guest.require(malformed_status != 0 and any(row['code'] == 'app_ambiguous' for row in malformed_reply.get('diagnostics', []))
                      and not d.probe(account, socket)['groups'], 'duplicate_fixture_not_rejected')
    finally:
        malformed.unlink()
    guest.require(accepted and count == 1, 'pinned_vendor_stock_expectation_changed')
    return {'version': (guest.ROOT / 'desktop-chrome-version').read_text().strip(),
            'source': 'vendor_debian_package', 'startup_wmclass_count': count,
            'stock_catalog_accepted': True, 'stock_registration_accepted': True,
            'malformed_fixture_duplicate_startup_wmclass_count': 2,
            'malformed_fixture_catalog_rejected': True, 'malformed_fixture_registration_rejected': True,
            'malformed_fixture_rejection': 'duplicate_recognized_key', 'fixture_catalog_origin': 'user_local',
            'fixture_launcher_kind': 'desktop_entry'}


def protocol(account, socket, role, kind):
    value = d.probe(account, socket)
    window = value['focused']
    if not window or window['protocol'] != kind or 'context_id' in window:
        if window and 'context_id' not in window:
            close_unregistered(account, socket)
        window = bootstrap(account, socket, role, kind)
    identity = {key: window[key] for key in d.IDENTITY_KEYS}
    if role == 'chrome' and kind == 'xwayland':
        # The vendor's actual X11 class is case-sensitive. Bind the disposable
        # entry to the exact canonical observation, as the connected fixture
        # does; never guess or weaken the resolver.
        guest.require(window['x11_class'], 'chrome_x11_class_missing')
        d.write_user(account, d.ENTRY, d.chrome_entry('x11').replace(
            'StartupWMClass=google-chrome\n', 'StartupWMClass=' + window['x11_class'] + '\n'))
    status, reply = d.cli_attempt(account, socket, *register_args(role), '--yes')
    accepted = status == 0
    if role == 'slack' and kind == 'xwayland':
        guest.require(not accepted and not identity['sandbox_app_id'] and identity['x11_class'] and identity['x11_instance'],
                      'expected_flatpak_xwayland_boundary')
    else:
        guest.require(accepted and len(reply.get('contexts', [])) == 1, 'protocol_registration_failed')
        context_id = reply['contexts'][0]['id']
        d.cli(account, socket, 'app', 'forget', '--yes', context_id)
    close_unregistered(account, socket)
    return {'identity': identity, 'mapped': True, 'registration_accepted': accepted}


def checkpoint(state, account, socket, phase, kind, actions, *, approval=False):
    screenshot = d.screenshot(account, socket, 'checkpoint')
    value = {'id': phase + '.' + uuid.uuid4().hex, 'kind': kind, 'private_screenshot': str(screenshot), 'actions': actions}
    state['pending'] = {'phase': phase, 'request': value, 'approval': approval}
    save(state)
    return {'complete': False, 'checkpoint': value}


def issue_gui(state, account, socket, phase):
    role = phase.split('.')[1]
    if group_exists := state['ids'].get(role):
        focus(account, socket, group_exists)
    else:
        value = d.probe(account, socket)['focused']
        if not value or value['protocol'] != 'wayland' or 'context_id' in value:
            bootstrap(account, socket, role)
    d.open_approval(account, socket, register_args(role))
    return checkpoint(state, account, socket, phase, 'approval',
                      ['approve'] if phase.endswith('.approve') else ['cancel'], approval=True)


def digest_for(context_id):
    rows = [(name, value) for name, value in d.approval_material().items() if name.startswith(context_id + '-')]
    guest.require(len(rows) == 1, 'expected_exact_context_approval_digest')
    return rows[0][1]


def inventory(account, socket, context_id):
    rows = d.cli(account, socket, 'app', 'list', '--inventory')['applications']
    found = [row for row in rows if row['context_id'] == context_id]
    guest.require(len(found) == 1, 'expected_context_inventory')
    return found[0]


def issue_trust(state, run_id, account, socket):
    import desktop_lifecycle as lifecycle
    context_id = state['ids']['chrome']
    d.cli(account, socket, 'app', 'pin', context_id)
    lifecycle.close_anchored(account, socket, context_id)
    before = digest_for(context_id)
    d.write_user(account, d.ENTRY, d.chrome_entry(changed=True))
    d.wait_for(lambda: inventory(account, socket, context_id)['launcher_validity']['state'] == 'reapproval_required')
    origin = trace.snapshot(run_id)
    d.cli(account, socket, 'restore', context_id)
    time.sleep(3)
    blocked = trace.delta(origin, trace.snapshot(run_id))
    guest.require(blocked['starts'] == {'chrome': 0, 'slack': 0}
                  and group(account, socket, context_id)['presence'] == 'absent', 'changed_launcher_was_launched')
    state['trust'] = {'stage': 'cancel', 'before': before, 'origin': origin}
    d.open_approval(account, socket, ['app', 'reapprove', context_id])
    return checkpoint(state, account, socket, 'trust.chrome.reapprove', 'approval', ['cancel'], approval=True)


def respond_trust(state, run_id, account, socket, action, x, y):
    context_id, trust = state['ids']['chrome'], state['trust']
    callbacks = trace.snapshot(run_id)['callbacks']
    if trust['stage'] == 'cancel':
        d.click(account, socket, x, y)
        d.wait_for(lambda: not d.swaynag_pids(account))
        guest.require(digest_for(context_id) == trust['before'] and trace.snapshot(run_id)['callbacks'] == callbacks,
                      'reapproval_cancel_changed_trust')
        trust['stage'] = 'approve'
        d.open_approval(account, socket, ['app', 'reapprove', context_id])
        return checkpoint(state, account, socket, 'trust.chrome.reapprove', 'approval', ['approve'], approval=True)
    if 'approved_digest' not in trust:
        trust['approve_callbacks_before'] = callbacks
        save(state)
        d.click(account, socket, x, y)
        after = d.wait_for(lambda: value if (value := digest_for(context_id)) != trust['before'] else None)
        guest.require(trace.snapshot(run_id)['callbacks'] > callbacks, 'reapproval_candidate_callback_missing')
        if d.swaynag_pids(account):
            d.click(account, socket, 1268, 16)
        d.wait_for(lambda: not d.swaynag_pids(account))
        trust['approved_digest'] = after
        save(state)
    after = trust['approved_digest']
    d.wait_for(lambda: inventory(account, socket, context_id)['launcher_validity']['state'] == 'valid')
    origin = trace.snapshot(run_id)
    explicit_reopen(account, socket, 'chrome', context_id)
    launches = trace.delta(origin, trace.snapshot(run_id))
    guest.require(launches['starts'] == {'chrome': 1, 'slack': 0}, 'reapproved_launcher_start_count')
    d.cli(account, socket, 'app', 'unpin', context_id)
    return passed({'material_changed': True, 'reapproval_required': True, 'blocked_launch_starts': 0,
                   'trace_complete': True, 'cancel_surface_mapped': True, 'cancel_visually_inspected': True,
                   'cancel_pointer_input': True, 'cancel_preserved_digest': True, 'approve_surface_mapped': True,
                   'approve_visually_inspected': True, 'approve_pointer_input': True, 'candidate_callback': True,
                   'approval_digest_before': trust['before'], 'approval_digest_after': after,
                   'launcher_valid_after': True, 'launch_starts_after': 1})


def ambiguity(state, run_id, account, socket):
    context_id = state['ids']['chrome']
    original = focus(account, socket, context_id)
    origin = trace.snapshot(run_id)
    # The headless seat gains keyboard capability when wtype connects. Give
    # the already focused application time to bind wl_keyboard before keys.
    d.user(account, ['wtype', '-s', '250', '-M', 'ctrl', '-s', '100', '-k', 'n',
                     '-s', '100', '-m', 'ctrl'], socket)
    windows = d.wait_for(lambda: value['windows'] if (value := group(account, socket, context_id))['window_count'] == 2 else None)
    created = [row['container_id'] for row in windows if row['container_id'] != original]
    guest.require(len(created) == 1, 'expected_exact_ui_created_window')
    guest.sway_command(account, socket, f'[con_id={original}] unmark persist:{context_id}')
    ambiguous = d.wait_for(lambda: value if (value := group(account, socket, context_id))['presence'] == 'ambiguous' else None)
    guest.require(not ambiguous['anchor_marked'] and ambiguous['anchor'] is None, 'ambiguity_guessed_anchor')
    modes = d.probe(account, socket)['capture_modes']
    guest.require({'workspace': '98', 'restore_mode': 'placement_only'} in modes, 'ambiguity_capture_not_degraded')
    guest.require(group(account, socket, state['ids']['slack'])['anchor_marked'], 'slack_anchor_lost')
    state['ambiguity'] = {'original': original, 'created': created[0], 'origin': origin}
    return checkpoint(state, account, socket, 'ambiguity.chrome', 'capability', ['approve'])


def respond_ambiguity(state, run_id, account, socket):
    value = state['ambiguity']
    measured = trace.delta(value['origin'], trace.snapshot(run_id))
    guest.require(measured['starts'] == {'chrome': 0, 'slack': 0}, 'multiwindow_duplicate_launch')
    facts = {'ui_inspected': True, 'window_count': 2, 'presence': 'ambiguous', 'anchor_marked': False,
             'known_fixture_mark_removed': True, 'restore_mode': 'placement_only', 'launch_trace': measured}
    context_id = state['ids']['chrome']
    current = group(account, socket, context_id)
    guest.require(current['presence'] == 'ambiguous' and current['window_count'] == 2
                  and {row['container_id'] for row in current['windows']} == {value['original'], value['created']},
                  'ambiguity_windows_changed')
    guest.sway_command(account, socket, f'[con_id={value["created"]}] focus')
    focused = d.probe(account, socket)['focused']
    guest.require(focused and focused['container_id'] == value['created'] and focused['workspace'] == '98',
                  'ui_created_window_focus_changed')
    guest.sway_command(account, socket, f'[con_id={value["created"]}] kill')
    d.wait_for(lambda: group(account, socket, context_id)['window_count'] == 1)
    guest.sway_command(account, socket, f'[con_id={value["original"]}] focus')
    d.cli(account, socket, *register_args('chrome'))
    anchor(account, socket, context_id)
    return passed(facts)


def capability(state, run_id, account, socket, phase):
    role = phase.split('.')[1]
    focus(account, socket, state['ids'][role])
    state['capability'] = {'origin': trace.snapshot(run_id), 'inspections': 0, 'samples': [],
                           'anchor': anchor(account, socket, state['ids'][role])}
    capability_samples(state, account, socket, phase, count=1)
    return checkpoint(state, account, socket, phase, 'capability', capability_actions(phase))


def capability_actions(phase):
    return ['ui-input', 'approve', 'not-exposed'] if phase.startswith('picker.') else ['ui-input', 'not-exposed']


def capability_samples(state, account, socket, phase, *, count=8):
    cap = state['capability']
    cap.setdefault('samples', [])
    role = phase.split('.')[1]
    context_id = state['ids'][role]
    for _ in range(count):
        probe = d.probe(account, socket)
        group = next(row for row in probe['groups'] if row['context_id'] == context_id)
        cap.setdefault('anchor', (group['anchor'] or {}).get('container_id'))
        cap['samples'].append({'presence': group['presence'], 'window_count': group['window_count'],
                               'total_windows': probe['total_windows'], 'desired_open': group['desired_open'],
                               'anchor_preserved': group['anchor_marked'] and (group['anchor'] or {}).get('container_id') == cap['anchor']})
        guest.require(len(cap['samples']) <= 256, 'capability_observation_budget')
        if count > 1:
            time.sleep(.1)


def transitions(samples, key):
    result = []
    for sample in samples:
        if not result or sample[key] != result[-1]:
            result.append(sample[key])
    guest.require(len(result) <= 16, 'capability_transition_budget')
    return result


def respond_capability(state, run_id, account, socket, phase, action, x, y):
    state['capability']['inspections'] += 1
    capability_samples(state, account, socket, phase, count=1)
    if action == 'ui-input':
        d.click(account, socket, x, y)
        capability_samples(state, account, socket, phase)
        return checkpoint(state, account, socket, phase, 'capability', capability_actions(phase))
    if action == 'approve':
        cap = state['capability']
        guest.require(phase.startswith('picker.') and cap['inspections'] >= 3, 'picker_requires_actual_ui_flow')
        measured = trace.delta(cap['origin'], trace.snapshot(run_id))
        guest.require(measured['starts'] == {'chrome': 0, 'slack': 0}, 'picker_duplicate_application_launch')
        samples = cap['samples']
        guest.require(all(row['desired_open'] and row['anchor_preserved'] and row['presence'] == 'present' for row in samples)
                      and samples[-1]['window_count'] == samples[0]['window_count'] == 1
                      and samples[-1]['total_windows'] == samples[0]['total_windows']
                      and max(row['total_windows'] for row in samples) == samples[0]['total_windows'] + 1,
                      'picker_main_anchor_or_intent_changed')
        return passed({'ui_inspected': True, 'empty_profile': True, 'picker_exposed': True,
                       'observed_presence_sequence': transitions(samples, 'presence'),
                       'observed_window_counts': transitions(samples, 'window_count'),
                       'observed_total_windows': transitions(samples, 'total_windows'),
                       'main_anchor_preserved': True, 'main_window_gap_observed': False,
                       'desired_open_preserved': True, 'samples': len(samples), 'launch_trace': measured})
    # The controller has inspected the actual available UI and explicitly
    # attests that the requested feature is absent in this empty profile.
    trace.delta(state['capability']['origin'], trace.snapshot(run_id))
    facts = {'ui_inspected': True, 'empty_profile': True,
             'second_window_exposed' if phase.startswith('ambiguity.') else 'picker_exposed': False,
             'automation_available': True}
    return {'status': 'not_exposed', 'reason': 'empty_profile_ui_absent', 'facts': facts}


def explicit_reopen(account, socket, role, context_id):
    observed = inventory(account, socket, context_id)
    guest.require(observed['launcher_validity']['state'] == 'valid', 'user_reopen_launcher_not_valid')
    if role == 'chrome':
        guest.require(guest.digest(d.ENTRY) == digest_for(context_id), 'user_reopen_differs_from_approved_snapshot')
        d.launch_entry(account, socket)
    else:
        launcher = observed['launcher']
        guest.require(launcher['kind'] == 'flatpak' and launcher['flatpak_id'] == 'com.slack.Slack'
                      and launcher['installation'] == 'user', 'unexpected_user_flatpak_launcher')
        guest.sway_command(account, socket, 'exec --no-startup-id /usr/bin/flatpak run --user com.slack.Slack')
    d.wait_for(lambda: group(account, socket, context_id)['presence'] == 'present', seconds=30)


def lifecycle_phase(state, run_id, account, socket, phase):
    import desktop_lifecycle as lifecycle
    family, role = phase.split('.')
    context_id = state['ids'][role]
    if group(account, socket, context_id)['presence'] == 'absent':
        explicit_reopen(account, socket, role, context_id)
    d.cli(account, socket, 'app', 'pin' if family == 'pinned' else 'unpin', context_id)
    return passed(getattr(lifecycle, family)(run_id, account, role, context_id))


def final_baseline(state, run_id, account, socket):
    import desktop_boot
    import desktop_lifecycle as lifecycle
    for role in ['chrome', 'slack']:
        context_id = state['ids'][role]
        if group(account, socket, context_id)['presence'] == 'present':
            d.cli(account, socket, 'app', 'pin', context_id)
            lifecycle.close_anchored(account, socket, context_id)
        guest.require(group(account, socket, context_id)['presence'] == 'absent', 'baseline_requires_absent_apps')
        d.cli(account, socket, 'app', 'unpin', context_id)
    desktop_boot.start_baseline_scope(run_id)
    for role in ['chrome', 'slack']:
        explicit_reopen(account, socket, role, state['ids'][role])
        d.wait_for(lambda: group(account, socket, state['ids'][role])['desired_open'] is True)
    state['complete'] = True


def session_socket(account):
    # SSH can start before the independent getty -> Sway -> routing publisher.
    # Retry only these read-only prerequisites, never a phase with effects.
    def ready():
        socket = guest.guest_socket(account)
        guest.desktop_environment(account, socket)
        guest.health(account, socket, candidate=d.CANDIDATE)
        return socket
    return guest.until_ready(ready, time.monotonic() + 60)


def run(run_id, account, *, checkpoint_id=None, action=None, x=None, y=None, inspected=False):
    socket = session_socket(account)
    d.pointer_ready(run_id, account, socket)
    if STATE.exists():
        guest.private_artifact(STATE)
        state = guest.read_json(STATE.read_text())
        guest.require(state['run_id'] == run_id, 'phase_state_run_mismatch')
    else:
        guest.require(not d.probe(account, socket)['groups'], 'phase_engine_requires_fresh_registry')
        state = {'run_id': run_id, 'phases': {}, 'ids': {}, 'next': 0, 'pending': None, 'complete': False}
    if state['pending']:
        pending = state['pending']
        if checkpoint_id is None:
            return {'complete': False, 'checkpoint': pending['request']}
        guest.require(checkpoint_id == pending['request']['id'] and action in pending['request']['actions'] and inspected,
                      'matching_inspected_checkpoint_required')
        phase = pending['phase']
        if phase.startswith('gui.'):
            role = phase.split('.')[1]
            result = passed(d.register_action(run_id, account, socket, role, approve=action == 'approve', x=x, y=y, inspected=True))
            if action == 'approve':
                state['ids'][role] = result['facts']['context_id']
        elif phase == 'trust.chrome.reapprove':
            result = respond_trust(state, run_id, account, socket, action, x, y)
        elif phase == 'ambiguity.chrome':
            result = respond_ambiguity(state, run_id, account, socket)
        else:
            result = respond_capability(state, run_id, account, socket, phase, action, x, y)
        if 'checkpoint' in result:
            return result
        state['phases'][phase] = result
        state['next'] += 1
        state['pending'] = None
        save(state)
    else:
        guest.require(checkpoint_id is None and action is None, 'unexpected_checkpoint_response')
    while not state['complete']:
        phase = ORDER[state['next']]
        if phase == 'stock.chrome':
            result = passed(stock(account, socket))
        elif phase.startswith('protocol.'):
            _, role, kind = phase.split('.')
            facts = protocol(account, socket, role, kind)
            result = passed(facts) if facts['registration_accepted'] else {
                'status': 'unsupported', 'reason': 'missing_sandbox_identity', 'facts': facts}
        elif phase.startswith('gui.'):
            return issue_gui(state, account, socket, phase)
        elif phase == 'trust.chrome.reapprove':
            return issue_trust(state, run_id, account, socket)
        elif phase == 'ambiguity.chrome':
            return ambiguity(state, run_id, account, socket)
        elif phase.startswith(('ambiguity.', 'picker.')):
            return capability(state, run_id, account, socket, phase)
        elif phase.startswith(('follow.', 'pinned.')):
            result = lifecycle_phase(state, run_id, account, socket, phase)
        else:
            final_baseline(state, run_id, account, socket)
            save(state)
            break
        state['phases'][phase] = result
        state['next'] += 1
        save(state)
    return {'complete': True, 'phases': state['phases'], 'expected_ids': [state['ids'][role] for role in ['chrome', 'slack']]}
