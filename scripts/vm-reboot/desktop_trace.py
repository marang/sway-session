#!/usr/bin/env python3
"""Guest-only fixed-launcher process observer; never reads argv or environment.

Slack's pinned Zypak wrapper starts the main ELF; its portal-launched sandbox
children execute that same ELF through a different ancestry. This classifier
accounts only for the installed, controlled normal launcher route. It cannot
identify an arbitrary portal request to run zypak-helper in host mode. Such
requests, optional spawn-latest reexec, and altered launch sources are outside
this fixture. Unknown origins fail rather than silently disappear.
"""
import argparse
import hashlib
import json
import os
import re
import selectors
import socket
import stat
import time
import uuid
from pathlib import Path
from typing import NamedTuple

import guest
from desktop_evidence import SOURCE_PINS

MEASUREMENT = 'sched_process_exec_fork_exit'
STATE = guest.ROOT / 'desktop-trace-state.json'
TRACE = Path('/sys/kernel/tracing')
MAX_EVENTS = 1000000
MAX_PROCESSES = 65536
CLASSIFICATION = 'fixed-normal-launchers-v1'
PORTAL = '/usr/lib/flatpak-portal'
ENTRY = '/app/bin/com.slack.Slack'
WRAPPERS = {'/app/bin/zypak-wrapper', '/app/bin/zypak-wrapper.sh'}
HELPER = '/app/bin/zypak-helper'
SLACK = '/app/extra/slack'
SOURCE_FILE = guest.ROOT / 'desktop-trace-sources.json'

# Primary implementation: Zypak v2025.09 at the source commit above, specifically
# zypak-wrapper.sh, src/base/launcher.cc, and the spawn_launcher_delegate.
# File hashes pin the actual installed package rather than a moving upstream ref.


def source_proof(value):
    guest.require(type(value) is dict and set(value) == set(SOURCE_PINS) | {'portal_sha256'},
                  'trace_source_proof_invalid')
    for key, expected in SOURCE_PINS.items():
        guest.require(type(value[key]) is type(expected) and value[key] == expected,
                      'trace_source_proof_invalid')
    guest.require(type(value['portal_sha256']) is str and
                  re.fullmatch(r'[0-9a-f]{64}', value['portal_sha256']) is not None,
                  'trace_source_proof_invalid')
    return dict(value)


def source_digest(path, *, root_owned=False, executable=False):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    with os.fdopen(descriptor, 'rb') as stream:
        info = os.fstat(stream.fileno())
        guest.require(stat.S_ISREG(info.st_mode) and not info.st_mode & 0o022
                      and 0 < info.st_size <= 2 * 1024 * 1024
                      and (not root_owned or info.st_uid == 0)
                      and (not executable or info.st_mode & 0o111),
                      'trace_source_file_invalid')
        data = stream.read(2 * 1024 * 1024 + 1)
        guest.require(len(data) == info.st_size, 'trace_source_file_invalid')
        return hashlib.sha256(data).hexdigest()


def validate_sources(files):
    """Read and pin public installed code only; performs no launches or writes."""
    try:
        files = Path(files)
        guest.require(files.is_absolute() and files.resolve() == files, 'trace_source_directory_invalid')
        guest.require(os.readlink(files / 'bin/zypak-wrapper') == '/app/bin/zypak-wrapper.sh',
                      'trace_source_alias_changed')
        proof = dict(SOURCE_PINS)
        for relative, key in [('manifest.json', 'manifest_sha256'),
                              ('bin/com.slack.Slack', 'entry_sha256'),
                              ('bin/zypak-wrapper.sh', 'wrapper_sha256'),
                              ('bin/zypak-helper', 'helper_sha256')]:
            digest = source_digest(files / relative, executable=relative != 'manifest.json')
            guest.require(digest == proof[key], 'trace_source_digest_changed')
        proof['portal_sha256'] = source_digest(Path(PORTAL), root_owned=True, executable=True)
        return source_proof(proof)
    except (OSError, ValueError, TypeError):
        raise RuntimeError('trace_sources_unavailable') from None


def deployment_files():
    return (guest.HOME / '.local/share/flatpak/app/com.slack.Slack/x86_64/stable'
            / SOURCE_PINS['slack_commit'] / 'files')


def load_sources():
    guest.private_artifact(SOURCE_FILE)
    guest.require(SOURCE_FILE.stat().st_size <= 4096, 'trace_source_proof_invalid')
    with SOURCE_FILE.open('rb') as stream:
        data = stream.read(4097)
    guest.require(len(data) <= 4096, 'trace_source_proof_invalid')
    try:
        saved = source_proof(json.loads(data))
    except (ValueError, TypeError, KeyError):
        raise RuntimeError('trace_source_proof_invalid') from None
    current = validate_sources(deployment_files())
    guest.require(saved == current, 'trace_source_digest_changed')
    return current


def require_no_portal():
    # Tracing must observe the portal executable incarnation before descendants
    # can inherit its provenance. Never seed from an unverified bare PID.
    visited = 0
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        visited += 1
        guest.require(visited <= MAX_PROCESSES, 'trace_process_budget')
        try:
            guest.require(os.readlink(entry / 'exe') != PORTAL, 'trace_portal_predates_recorder')
        except FileNotFoundError:
            continue


class Process(NamedTuple):
    role: str = ''
    approval: bool = False
    portal: bool = False
    entry: bool = False
    wrapper: bool = False
    helper: bool = False


class Incarnations:
    def __init__(self, sources=None):
        self.processes = {}
        self.starts = {'chrome': 0, 'slack': 0}
        self.helpers = {'slack': 0}
        self.unclassified = 0
        self.callbacks = 0
        self.events = 0
        self.barrier = ''
        self.sources = source_proof(sources) if sources is not None else None

    def consume(self, line):
        self.events += 1
        guest.require(self.events <= MAX_EVENTS, 'trace_event_budget')
        if 'tracing_mark_write:' in line:
            value = line.split('tracing_mark_write:', 1)[1].strip()
            if re.fullmatch(r'lab253:[0-9a-f-]{36}', value):
                self.barrier = value[7:]
            return
        if 'sched_process_fork:' in line:
            value = re.search(r' pid=(\d+) child_comm=.* child_pid=(\d+)\s*$', line)
            guest.require(value is not None, 'trace_fork_shape')
            parent, child = map(int, value.groups())
            self.processes[child] = self.processes.get(parent, Process())
        elif 'sched_process_exec:' in line:
            value = re.search(r' filename=(\S+) pid=(\d+) old_pid=(\d+)\s*$', line)
            guest.require(value is not None, 'trace_exec_shape')
            filename, pid, old = value.group(1), int(value.group(2)), int(value.group(3))
            process = self.processes.pop(old, Process()) if old != pid else self.processes.get(pid, Process())
            if filename == PORTAL:
                process = Process(portal=True)
            elif filename == ENTRY:
                process = process._replace(entry=True, wrapper=False, helper=False)
            elif filename in WRAPPERS:
                process = process._replace(wrapper=process.entry, helper=False)
            elif filename == HELPER:
                process = process._replace(helper=True)
            if filename == SLACK:
                if process.role == 'slack':
                    pass  # Reexec or descendant of the same main incarnation.
                elif process.entry and process.wrapper and process.helper:
                    self.starts['slack'] += 1
                    process = process._replace(role='slack')
                elif (process.role == 'slack_helper'
                      or process.portal and process.helper and not process.entry and not process.wrapper):
                    self.helpers['slack'] += 1
                    process = process._replace(role='slack_helper')
                else:
                    self.unclassified += 1
                    raise RuntimeError('trace_slack_origin_unclassified')
            elif filename == '/opt/google/chrome/chrome' and not process.role:
                self.starts['chrome'] += 1
                process = process._replace(role='chrome')
            if filename == '/usr/bin/sway-session' and process.approval:
                self.callbacks += 1
            process = process._replace(approval=process.approval or filename == '/usr/bin/swaynag')
            self.processes[pid] = process
        elif 'sched_process_exit:' in line:
            value = re.search(r' pid=(\d+) prio=', line)
            guest.require(value is not None, 'trace_exit_shape')
            self.processes.pop(int(value.group(1)), None)
        elif line.strip() and not line.startswith('#'):
            raise RuntimeError('trace_unexpected_event')
        guest.require(len(self.processes) <= MAX_PROCESSES, 'trace_process_budget')

    def state(self, boot_id):
        guest.require(self.sources is not None and self.unclassified == 0, 'trace_sources_missing')
        return {'boot_id': boot_id, 'starts': dict(self.starts), 'callbacks': self.callbacks,
                'events': self.events, 'barrier': self.barrier, 'measurement': MEASUREMENT,
                'classification': CLASSIFICATION, 'helpers': dict(self.helpers),
                'unclassified': self.unclassified, 'sources': dict(self.sources)}


def instance(run_id):
    return TRACE / 'instances' / ('lab253-' + run_id)


def overrun(root):
    rows = list((root / 'per_cpu').glob('cpu*/stats'))
    guest.require(rows, 'trace_cpu_stats_missing')
    for path in rows:
        stats = dict(line.split(':', 1) for line in path.read_text().splitlines() if ':' in line)
        for field in ['overrun', 'commit overrun', 'dropped events']:
            guest.require(field in stats and int(stats[field]) == 0, 'trace_overrun')


def write_state(value):
    target = STATE.with_suffix('.new')
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        json.dump(value, output)
    os.replace(target, STATE)


def serve(run_id):
    if not (TRACE / 'trace').exists():
        guest.command(['mount', '-t', 'tracefs', 'nodev', TRACE])
    sources = load_sources()
    require_no_portal()
    root = instance(run_id)
    guest.require(not root.exists(), 'trace_instance_already_exists')
    root.mkdir()
    for name, value in [('tracing_on', '0'), ('current_tracer', 'nop'),
                        ('trace_clock', 'global'), ('buffer_size_kb', '8192')]:
        (root / name).write_text(value)
    (root / 'options/overwrite').write_text('0')
    for name in ['sched_process_exec', 'sched_process_fork', 'sched_process_exit']:
        path = root / 'events/sched' / name / 'enable'
        guest.require(path.exists(), 'trace_required_event_missing')
        path.write_text('1')
    boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    recorder = Incarnations(sources)
    write_state(recorder.state(boot))
    with (root / 'trace_pipe').open('rb', buffering=0) as stream, selectors.DefaultSelector() as selector:
        os.set_blocking(stream.fileno(), False)
        selector.register(stream, selectors.EVENT_READ)
        pending = b''
        (root / 'tracing_on').write_text('1')
        # Type=notify ensures getty can never race ahead of tracing readiness.
        notify = os.environ.get('NOTIFY_SOCKET', '')
        guest.require(notify == '/run/systemd/notify' or re.fullmatch(r'@/org/freedesktop/systemd1/notify/[a-zA-Z0-9]+', notify),
                      'trace_notify_socket_invalid')
        with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as peer:
            peer.sendto(b'READY=1', '\0' + notify[1:] if notify.startswith('@') else notify)
        while True:
            for _key, _event in selector.select(1):
                pending += os.read(stream.fileno(), 65536)
                guest.require(len(pending) <= 262144, 'trace_line_budget')
                lines = pending.split(b'\n')
                pending = lines.pop()
                for line in lines:
                    recorder.consume(line.decode('utf-8', errors='strict'))
                overrun(root)
                write_state(recorder.state(boot))


def snapshot(run_id):
    sources = load_sources()
    root = instance(run_id)
    guest.require(root.exists() and (root / 'tracing_on').read_text().strip() == '1', 'trace_not_running')
    marker = str(uuid.uuid4())
    (root / 'trace_marker').write_text('lab253:' + marker)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        guest.private_artifact(STATE)
        value = guest.read_json(STATE.read_text())
        if value['barrier'] == marker:
            guest.require(value['sources'] == sources and value['classification'] == CLASSIFICATION
                          and value['unclassified'] == 0, 'trace_source_proof_invalid')
            overrun(root)
            guest.require(value['boot_id'] == Path('/proc/sys/kernel/random/boot_id').read_text().strip(), 'trace_boot_mismatch')
            return value
        time.sleep(.02)
    raise RuntimeError('trace_barrier_timeout')


def install(run_id, *, slack_files):
    guest.require(Path(slack_files) == deployment_files(), 'trace_source_directory_invalid')
    sources = validate_sources(slack_files)
    require_no_portal()
    guest.require(not SOURCE_FILE.exists(), 'trace_sources_already_installed')
    with SOURCE_FILE.open('x', encoding='utf-8') as stream:
        json.dump(sources, stream)
    SOURCE_FILE.chmod(0o600)
    unit = Path('/etc/systemd/system/lab253-process-trace.service')
    guest.require(not unit.exists(), 'trace_service_already_installed')
    unit.write_text('[Unit]\nDescription=Owned LAB253 guest process observer\nBefore=getty@tty1.service\n'
                    '[Service]\nType=notify\nNotifyAccess=main\nExecStart=/usr/bin/python /opt/sway-session-test/desktop_trace.py serve\n'
                    'StandardOutput=null\nStandardError=null\nUMask=0077\n'
                    '[Install]\nRequiredBy=getty@tty1.service\n')
    guest.command(['systemctl', 'daemon-reload'])
    guest.command(['systemctl', 'enable', '--now', 'lab253-process-trace.service'])
    snapshot(run_id)


def delta(before, after):
    for value in (before, after):
        guest.require(type(value['events']) is int and 0 <= value['events'] <= MAX_EVENTS
                      and type(value['starts']) is dict and set(value['starts']) == {'chrome', 'slack'}
                      and type(value['helpers']) is dict and set(value['helpers']) == {'slack'}
                      and all(type(count) is int and 0 <= count <= MAX_EVENTS
                              for count in [*value['starts'].values(), *value['helpers'].values()]),
                      'trace_counter_invalid')
    guest.require(before['boot_id'] == after['boot_id'] and after['events'] >= before['events'], 'trace_scope_mismatch')
    guest.require(before['classification'] == after['classification'] == CLASSIFICATION
                  and before['sources'] == after['sources'] and before['unclassified'] == after['unclassified'] == 0,
                  'trace_source_proof_invalid')
    sources = source_proof(after['sources'])
    starts = {role: after['starts'][role] - before['starts'][role] for role in ['chrome', 'slack']}
    helpers = {'slack': after['helpers']['slack'] - before['helpers']['slack']}
    guest.require(all(type(value) is int and value >= 0 for value in [*starts.values(), *helpers.values()]),
                  'trace_counter_regression')
    return {'complete': True, 'measurement': MEASUREMENT, 'starts': starts,
            'classification': CLASSIFICATION, 'helpers': helpers, 'unclassified': 0, 'sources': sources}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['serve', 'snapshot'])
    args = parser.parse_args()
    run_id, _account = guest.guard()
    if args.command == 'serve':
        serve(run_id)
    else:
        print(json.dumps(snapshot(run_id)))


if __name__ == '__main__':
    main()
