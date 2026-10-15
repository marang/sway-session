#!/usr/bin/env python3
"""Guest-only bounded process-incarnation observer. Never records argv or env.

The kernel sched trace definitions provide filename/pid/old_pid for exec and
parent_pid/child_pid for fork. A descendant inherits its application incarnation,
so helpers and reexecs cannot masquerade as another application root. A second
independent root counts even if it exits before a compositor observation.
See https://docs.kernel.org/trace/ftrace.html and include/trace/events/sched.h.
"""
import argparse
import json
import os
import re
import selectors
import socket
import time
import uuid
from pathlib import Path

import guest

MEASUREMENT = 'sched_process_exec_fork_exit'
STATE = guest.ROOT / 'desktop-trace-state.json'
TRACE = Path('/sys/kernel/tracing')
MAX_EVENTS = 1000000
MAX_PROCESSES = 65536
EXECUTABLES = {'/opt/google/chrome/chrome': 'chrome',
               '/app/extra/slack/slack': 'slack'}


class Incarnations:
    def __init__(self):
        self.processes = {}
        self.starts = {'chrome': 0, 'slack': 0}
        self.callbacks = 0
        self.events = 0
        self.barrier = ''

    def consume(self, line):
        self.events += 1
        guest.require(self.events <= MAX_EVENTS, 'trace_event_budget')
        # Metadata before the colon contains task comm; it stays private and is
        # deliberately neither interpreted as identity nor copied into state.
        if 'tracing_mark_write:' in line:
            value = line.split('tracing_mark_write:', 1)[1].strip()
            if re.fullmatch(r'lab253:[0-9a-f-]{36}', value):
                self.barrier = value[7:]
            return
        if 'sched_process_fork:' in line:
            value = re.search(r' pid=(\d+) child_comm=.* child_pid=(\d+)\s*$', line)
            guest.require(value is not None, 'trace_fork_shape')
            parent, child = map(int, value.groups())
            self.processes[child] = self.processes.get(parent, ('', False))
        elif 'sched_process_exec:' in line:
            value = re.search(r' filename=(\S+) pid=(\d+) old_pid=(\d+)\s*$', line)
            guest.require(value is not None, 'trace_exec_shape')
            filename, pid, old = value.group(1), int(value.group(2)), int(value.group(3))
            inherited = self.processes.pop(old, ('', False)) if old != pid else self.processes.get(pid, ('', False))
            role, approval = inherited
            actual = EXECUTABLES.get(filename, '')
            if actual and not role:
                self.starts[actual] += 1
                role = actual
            if filename == '/usr/bin/sway-session' and approval:
                self.callbacks += 1
            self.processes[pid] = (role, approval or filename == '/usr/bin/swaynag')
        elif 'sched_process_exit:' in line:
            value = re.search(r' pid=(\d+) prio=', line)
            guest.require(value is not None, 'trace_exit_shape')
            self.processes.pop(int(value.group(1)), None)
        elif line.strip() and not line.startswith('#'):
            raise RuntimeError('trace_unexpected_event')
        guest.require(len(self.processes) <= MAX_PROCESSES, 'trace_process_budget')

    def state(self, boot_id):
        return {'boot_id': boot_id, 'starts': self.starts, 'callbacks': self.callbacks,
                'events': self.events, 'barrier': self.barrier, 'measurement': MEASUREMENT}


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
    recorder = Incarnations()
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
    root = instance(run_id)
    guest.require(root.exists() and (root / 'tracing_on').read_text().strip() == '1', 'trace_not_running')
    marker = str(uuid.uuid4())
    (root / 'trace_marker').write_text('lab253:' + marker)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        guest.private_artifact(STATE)
        value = guest.read_json(STATE.read_text())
        if value['barrier'] == marker:
            overrun(root)
            guest.require(value['boot_id'] == Path('/proc/sys/kernel/random/boot_id').read_text().strip(), 'trace_boot_mismatch')
            return value
        time.sleep(.02)
    raise RuntimeError('trace_barrier_timeout')


def install(run_id):
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
    guest.require(before['boot_id'] == after['boot_id'] and after['events'] >= before['events'], 'trace_scope_mismatch')
    starts = {role: after['starts'][role] - before['starts'][role] for role in ['chrome', 'slack']}
    guest.require(all(value >= 0 for value in starts.values()), 'trace_counter_regression')
    return {'complete': True, 'measurement': MEASUREMENT, 'starts': starts}


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
