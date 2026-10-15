#!/usr/bin/env python3
"""Join structural desktop captures with real guest boot and launch evidence.

This module never launches applications or changes their saved policy. The
phase controller closes both apps, starts the single baseline trace scope, then
explicitly reopens them before asking this collector for the original baseline.
"""
import argparse
import json
import os
import sys
import time
import uuid
from pathlib import Path

import desktop_evidence
import desktop_guest
import desktop_trace
import guest

CANDIDATE = Path('/usr/bin/sway-session')
SCOPE = guest.ROOT / 'desktop-baseline-scope.json'
BOOT_ID = Path('/proc/sys/kernel/random/boot_id')
TIMEOUT = 160
INTERVAL = 1
STABLE_SAMPLES = 3
FAILURE = 'desktop_boot_prerequisite_or_assertion'


def require(condition):
    if not condition:
        raise RuntimeError(FAILURE)


def canonical_uuid(value, *, v4=False):
    require(type(value) is str)
    parsed = uuid.UUID(value)
    require(str(parsed) == value and (not v4 or parsed.version == 4))
    return value


def expected_ids(values):
    require(type(values) is list and len(values) == 2)
    result = [canonical_uuid(value) for value in values]
    require(result[0] != result[1])
    return result


def guarded(run_id, account=None):
    current, owner = guest.guard()
    require(canonical_uuid(run_id, v4=True) == current)
    if account is not None:
        require(account == owner)
    return owner


def boot_id():
    return canonical_uuid(BOOT_ID.read_text().strip())


def checked_snapshot(value):
    require(type(value) is dict and set(value) == {
        'boot_id', 'starts', 'callbacks', 'events', 'barrier', 'measurement',
        'classification', 'helpers', 'unclassified', 'sources'})
    canonical_uuid(value['boot_id'])
    canonical_uuid(value['barrier'], v4=True)
    require(value['measurement'] == desktop_trace.MEASUREMENT
            and type(value['starts']) is dict and set(value['starts']) == {'chrome', 'slack'})
    require(value['classification'] == desktop_evidence.CLASSIFICATION
            and type(value['helpers']) is dict and set(value['helpers']) == {'slack'}
            and type(value['helpers']['slack']) is int and 0 <= value['helpers']['slack'] <= 1000000
            and type(value['unclassified']) is int and value['unclassified'] == 0)
    desktop_evidence._sources(value['sources'])
    for number in [*value['starts'].values(), value['callbacks'], value['events']]:
        require(type(number) is int and 0 <= number <= 1000000)
    return value


def trace_snapshot(run_id):
    value = checked_snapshot(desktop_trace.snapshot(run_id))
    require(value['boot_id'] == boot_id())
    return value


def load_scope(run_id):
    guest.private_artifact(SCOPE)
    info = SCOPE.lstat()
    require(not info.st_mode & 0o077 and info.st_size <= 16384)
    value = guest.read_json(SCOPE.read_text())
    require(type(value) is dict and set(value) == {
        'version', 'run_id', 'boot_id', 'expected_ids', 'snapshot'})
    require(type(value['version']) is int and value['version'] == 1 and value['run_id'] == run_id)
    canonical_uuid(value['boot_id'])
    expected_ids(value['expected_ids'])
    snapshot = checked_snapshot(value['snapshot'])
    require(snapshot['boot_id'] == value['boot_id'])
    return value


def registered_ids(account, socket):
    # Role selection uses the production inventory's typed launcher identity.
    # No compositor identity comparison is reimplemented in Python.
    inventory = desktop_guest.cli(account, socket, 'app', 'list', '--inventory', '--socket', socket)
    applications = inventory['applications']
    require(type(applications) is list and len(applications) == 2)
    roles = {}
    for item in applications:
        launcher = item['launcher']
        role = None
        if launcher.get('kind') == 'desktop' and launcher.get('desktop_id') == 'lab253-chrome.desktop':
            role = 'chrome'
        if launcher.get('kind') == 'flatpak' and launcher.get('flatpak_id') == 'com.slack.Slack' and launcher.get('installation') == 'user':
            role = 'slack'
        require(role is not None and role not in roles and item['state'] == 'active')
        roles[role] = canonical_uuid(item['context_id'])
    return expected_ids([roles['chrome'], roles['slack']])


def start_baseline_scope(run_id):
    account = guarded(run_id)
    harness = guest.load_harness(run_id)
    require(harness['baseline_started'] is False and harness['expected_ids'] == [])
    require(not SCOPE.exists() and not SCOPE.is_symlink())
    socket = guest.guest_socket(account)
    ids = registered_ids(account, socket)
    probe = desktop_guest.probe(account, socket)
    require(probe['scenario'] == 'desktop_probe' and type(probe['total_windows']) is int
            and probe['total_windows'] == 0 and probe['focused'] is None
            and probe['capture_modes'] == [] and len(probe['groups']) == 2)
    require(sorted(group['context_id'] for group in probe['groups']) == sorted(ids))
    for group in probe['groups']:
        require(group['state'] == 'active' and group['presence'] == 'absent'
                and type(group['window_count']) is int and group['window_count'] == 0
                and group['anchor'] is None and group['anchor_marked'] is False and group['windows'] == [])
    snapshot = trace_snapshot(run_id)
    value = {'version': 1, 'run_id': run_id, 'boot_id': snapshot['boot_id'],
             'expected_ids': ids, 'snapshot': snapshot}
    # O_EXCL makes this an immutable measurement origin for the fixture run;
    # retries cannot erase launches by replacing the scope after reopening.
    descriptor = os.open(SCOPE, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        json.dump(value, output, separators=(',', ':'))
        output.write('\n')
    return {'scope_started': True}


def launch_trace(run_id, scope):
    current = trace_snapshot(run_id)
    if current['boot_id'] == scope['boot_id']:
        return desktop_trace.delta(scope['snapshot'], current)
    # The root guest recorder is armed before getty on cold boots. These are
    # actual total observed root incarnations, not durable launch-guard rows.
    return {'complete': True, 'measurement': desktop_trace.MEASUREMENT,
            'starts': dict(current['starts']), 'classification': current['classification'],
            'helpers': dict(current['helpers']), 'unclassified': current['unclassified'],
            'sources': dict(current['sources'])}


def sample(run_id, account, harness, scope):
    socket = guest.guest_socket(account)
    guest.private_artifact(guest.ROOT / 'observe', executable=True)
    observation = guest.read_json(desktop_guest.user(account, [guest.ROOT / 'observe',
        '--scenario', 'desktop', '--state-root', guest.HOME / '.local/state/sway-session',
        '--socket', socket, '--expected', ','.join(harness['expected_ids'])], socket))
    observation.update(guest.health(account, socket, candidate=CANDIDATE))
    artifact = guest.artifact_identity(candidate=CANDIDATE)
    require(artifact == harness['artifact'])
    observation.update({'run_id': run_id, 'boot_id': boot_id(), 'artifact': artifact,
                        'versions': desktop_guest.versions(account, socket),
                        'launch_trace': launch_trace(run_id, scope)})
    return observation


def stable_observation(run_id, account, harness, scope):
    deadline = time.monotonic() + TIMEOUT
    previous, count = None, 0
    while time.monotonic() < deadline:
        try:
            value = sample(run_id, account, harness, scope)
            count = count + 1 if value == previous else 1
            previous = value
            if count >= STABLE_SAMPLES:
                value['stable_samples'] = count
                desktop_evidence.validate_desktop_baseline(value)
                return value
        except (RuntimeError, OSError, ValueError, KeyError, TypeError):
            previous, count = None, 0
        time.sleep(INTERVAL)
    raise RuntimeError(FAILURE)


def baseline(run_id, account, ids):
    guarded(run_id, account)
    ids = expected_ids(ids)
    scope = load_scope(run_id)
    require(scope['expected_ids'] == ids and scope['boot_id'] == boot_id())
    harness = guest.load_harness(run_id)
    require(type(harness['baseline_started']) is bool)
    if harness['baseline_started']:
        require(harness['expected_ids'] == ids)
    else:
        require(harness['expected_ids'] == [])
        harness['baseline_started'] = True
        harness['expected_ids'] = ids
        guest.write_harness(harness)
    socket = guest.guest_socket(account)
    # The controller already opened both apps on workspace 98 in role order.
    # No move, manual capture, registry write, restore or lifecycle effect occurs.
    guest.sway_command(account, socket, 'workspace 98; layout tabbed')
    return stable_observation(run_id, account, harness, scope)


def observe(run_id, account):
    guarded(run_id, account)
    harness = guest.load_harness(run_id)
    scope = load_scope(run_id)
    require(harness['baseline_started'] is True
            and expected_ids(harness['expected_ids']) == scope['expected_ids'])
    return stable_observation(run_id, account, harness, scope)


class Parser(argparse.ArgumentParser):
    def error(self, _message):
        raise RuntimeError(FAILURE)


def main(arguments=None):
    try:
        parser = Parser(description=__doc__)
        parser.add_argument('operation', choices=['start-baseline-scope', 'baseline', 'observe'])
        parser.add_argument('--expected')
        args = parser.parse_args(arguments)
        run_id, account = guest.guard()
        if args.operation == 'start-baseline-scope':
            require(args.expected is None)
            value = start_baseline_scope(run_id)
        elif args.operation == 'baseline':
            require(args.expected is not None)
            value = baseline(run_id, account, args.expected.split(','))
        else:
            require(args.expected is None)
            value = observe(run_id, account)
        encoded = json.dumps(value, separators=(',', ':'))
        require(len(encoded.encode()) <= desktop_evidence.LIMIT)
        print(encoded)
        return 0
    except (Exception, KeyboardInterrupt):
        # Subprocess/Doctor/JSON failures can carry private content. Export only
        # this fixed typed rejection and never an exception or traceback.
        print(json.dumps({'failed': True, 'reason': FAILURE}, separators=(',', ':')))
        return 1


if __name__ == '__main__':
    sys.exit(main())
