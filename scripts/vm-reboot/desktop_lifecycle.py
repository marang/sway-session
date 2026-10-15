#!/usr/bin/env python3
"""Observe real desktop close policy in the owned LAB253 KVM guest only.

The controller sets policy and opens applications. These helpers close an exact
canonical anchor and observe production wall-clock behavior; they never restore
or reopen an app. Only the attributed candidate daemon may be restarted.
"""
import functools
import json
import os
from pathlib import Path
import re
import select
import signal
import socket as sockets
import struct
import time

import desktop_boot
import desktop_evidence
import desktop_guest
import desktop_trace
import guest

CANDIDATE = Path('/usr/bin/sway-session')
GRACE_MS = 2000
INTERVAL = .25
TIMEOUT = 40
MAX_EVENTS = 256
FRAME_LIMIT = 1024 * 1024
WINDOW_EVENT = (1 << 31) | 3
FAILURE = 'desktop_lifecycle_prerequisite_or_assertion'


def require(ok):
    if not ok:
        raise RuntimeError(FAILURE)


def redacted(operation):
    @functools.wraps(operation)
    def invoke(*args, **kwargs):
        try:
            return operation(*args, **kwargs)
        except Exception:
            raise RuntimeError(FAILURE) from None
    return invoke


class WindowEvents:
    """Bounded i3 IPC subscription, matching internal/swayipc/protocol.go.

    14-byte i3-ipc header, little-endian size/type; subscribe=2, window=bit31|3.
    Browser titles and all other event payload fields remain private.
    """
    def __init__(self, path):
        self.path = path
        self.peer = None

    def __enter__(self):
        self.peer = sockets.socket(sockets.AF_UNIX, sockets.SOCK_STREAM)
        try:
            self.peer.settimeout(10)
            self.peer.connect(str(self.path))
            payload = b'["window"]'
            self.peer.sendall(b'i3-ipc' + struct.pack('<II', len(payload), 2) + payload)
            kind, response = self.frame(time.monotonic() + 10)
            require(kind == 2 and type(response) is dict and set(response) == {'success'}
                    and response['success'] is True)
            return self
        except BaseException:
            self.peer.close()
            raise

    def __exit__(self, *_args):
        self.peer.close()

    def exact(self, length, deadline):
        result = bytearray()
        while len(result) < length:
            remaining = deadline - time.monotonic()
            require(remaining > 0)
            self.peer.settimeout(remaining)
            piece = self.peer.recv(length - len(result))
            require(bool(piece))
            result.extend(piece)
        return bytes(result)

    def frame(self, deadline):
        header = self.exact(14, deadline)
        require(header[:6] == b'i3-ipc')
        length, kind = struct.unpack('<II', header[6:])
        require(0 < length <= FRAME_LIMIT)
        return kind, json.loads(self.exact(length, deadline))

    def closed(self, container_id):
        deadline = time.monotonic() + 10
        for _ in range(MAX_EVENTS):
            kind, event = self.frame(deadline)
            require(kind == WINDOW_EVENT and type(event) is dict)
            if event.get('change') == 'close':
                node = event.get('container')
                require(type(node) is dict and type(node.get('id')) is int)
                if node['id'] == container_id:
                    return True
        raise RuntimeError(FAILURE)


def compositor_pid(account, socket):
    require(socket.parent == Path('/run/user') / str(account.pw_uid))
    match = re.fullmatch(r'sway-ipc\.' + str(account.pw_uid) + r'\.([1-9][0-9]*)\.sock', socket.name)
    require(match is not None)
    pid = int(match[1])
    require(pid <= 4194304)
    owned_executable(account, pid, Path('/usr/bin/sway'))
    return pid


def owned_executable(account, pid, executable):
    process = Path('/proc') / str(pid)
    require(process.stat().st_uid == account.pw_uid and (process / 'exe').resolve() == executable)


def healthy(run_id, account, socket):
    value = guest.health(account, socket, candidate=CANDIDATE)
    desktop_evidence._health(value)
    require(value['inhibitor']['uid'] == account.pw_uid
            and guest.artifact_identity(candidate=CANDIDATE) == guest.load_harness(run_id)['artifact'])
    return value


def group_for(account, socket, context_id):
    value = desktop_guest.probe(account, socket)
    require(value['scenario'] == 'desktop_probe')
    matches = [group for group in value['groups'] if group['context_id'] == context_id]
    require(len(matches) == 1)
    return value, matches[0]


def anchor_for(account, socket, context_id):
    probe, group = group_for(account, socket, context_id)
    require(group['state'] == 'active' and group['presence'] == 'present'
            and type(group['window_count']) is int and group['window_count'] == 1
            and group['anchor_marked'] is True and type(group['anchor']) is dict)
    anchor = group['anchor']
    require(type(anchor['container_id']) is int and 0 < anchor['container_id'] <= 2147483647
            and anchor['workspace'] == '98' and group['windows'] == [anchor])
    return probe, group, anchor


@redacted
def close_anchored(account, socket, context_id):
    run_id, owner = guest.guard()
    require(account == owner and socket == guest.guest_socket(account))
    ids = desktop_boot.registered_ids(account, socket)
    require(context_id in ids)
    healthy(run_id, account, socket)
    probe, _group, anchor = anchor_for(account, socket, context_id)
    # Inspect focus before touching the known anchor, then re-observe focus and
    # membership. No title, class fallback or arbitrary window choice is used.
    require('focused' in probe)
    with WindowEvents(socket) as events:
        guest.sway_command(account, socket, '[con_id=' + str(anchor['container_id']) + '] focus')
        probe, _group, current = anchor_for(account, socket, context_id)
        require(current == anchor and type(probe['focused']) is dict
                and probe['focused']['container_id'] == anchor['container_id']
                and probe['focused']['context_id'] == context_id and probe['focused']['workspace'] == '98')
        guest.sway_command(account, socket, '[con_id=' + str(anchor['container_id']) + '] kill')
        require(events.closed(anchor['container_id']) is True)
    return {'container_id': anchor['container_id'], 'fresh_close_event': True}


def prepared(run_id, account, role, context_id, policy):
    desktop_boot.guarded(run_id, account)
    require(role in ('chrome', 'slack'))
    socket = guest.guest_socket(account)
    ids = desktop_boot.registered_ids(account, socket)
    require(context_id == ids[('chrome', 'slack').index(role)])
    _probe, group, _anchor = anchor_for(account, socket, context_id)
    require(group['policy'] == policy and group['desired_open'] is True)
    health = healthy(run_id, account, socket)
    return socket, ids, health, compositor_pid(account, socket)


def absent(run_id, account, socket, context_id, policy, compositor, daemon_pid):
    deadline = time.monotonic() + TIMEOUT
    since, count = None, 0
    previous, latest = None, None
    while time.monotonic() < deadline:
        require(guest.guest_socket(account) == socket and compositor_pid(account, socket) == compositor)
        latest = healthy(run_id, account, socket)
        require(latest['daemon_pid'] == daemon_pid)
        _probe, group = group_for(account, socket, context_id)
        require(group['state'] == 'active' and group['policy'] == policy)
        missing = (group['presence'] == 'absent' and type(group['window_count']) is int
                   and group['window_count'] == 0 and group['anchor'] is None
                   and group['anchor_marked'] is False and group['windows'] == [])
        if missing:
            if since is None:
                since = time.monotonic()
            desired = policy == 'pinned'
            if group['desired_open'] is desired:
                count = count + 1 if group == previous else 1
                previous = group
                elapsed = int((time.monotonic() - since) * 1000)
                if elapsed > GRACE_MS and count >= 3:
                    require(count <= 120)
                    return elapsed, count, latest
            else:
                previous, count = None, 0
        else:
            # Allow only compositor event propagation before the first absence.
            # A later reappearance is a failed lifecycle claim, even if it closes.
            require(since is None)
        time.sleep(INTERVAL)
    raise RuntimeError(FAILURE)


def zero_trace(run_id, before):
    trace = desktop_trace.delta(before, desktop_boot.trace_snapshot(run_id))
    desktop_evidence._trace(trace, 0)
    return trace


def restart_daemon(run_id, account, socket, old_pid):
    # pidfd targets precisely the Doctor-attributed process, including if its
    # numeric PID exits/recycles during checks. Never scan or signal by name.
    descriptor = os.pidfd_open(old_pid, 0)
    try:
        require(healthy(run_id, account, socket)['daemon_pid'] == old_pid)
        owned_executable(account, old_pid, CANDIDATE)
        signal.pidfd_send_signal(descriptor, signal.SIGTERM)
        require(bool(select.select([descriptor], [], [], 10)[0]))
    finally:
        os.close(descriptor)
    # Sway's exec inherits the original getty/PAM/logind session. No systemd-run
    # or restore command can accidentally create a different lifecycle boundary.
    guest.sway_command(account, socket, 'exec --no-startup-id /usr/bin/sway-session daemon')
    deadline = time.monotonic() + TIMEOUT
    while time.monotonic() < deadline:
        try:
            current = healthy(run_id, account, socket)
            require(current['daemon_pid'] != old_pid)
            return current['daemon_pid']
        except (RuntimeError, OSError, ValueError, KeyError):
            time.sleep(INTERVAL)
    raise RuntimeError(FAILURE)


def finish(policy, role, ids, facts):
    wrapper = {'status': 'passed', 'reason': 'verified', 'facts': facts}
    desktop_evidence._bounded(wrapper)
    desktop_evidence._phase(policy + '.' + role, wrapper, ids, {})
    return facts


@redacted
def follow(run_id, account, role, context_id):
    socket, ids, health, compositor = prepared(run_id, account, role, context_id, 'follow')
    before = desktop_boot.trace_snapshot(run_id)
    close_anchored(account, socket, context_id)
    elapsed, count, health = absent(run_id, account, socket, context_id, 'follow', compositor, health['daemon_pid'])
    return finish('follow', role, ids, {'context_id': context_id, 'context_state': 'active', 'policy': 'follow',
        'present_before': True, 'fresh_close_event': True, 'absent_after': True,
        'absence_ms': elapsed, 'production_close_grace_ms': GRACE_MS, 'steady_samples': count,
        'desired_open': False, 'health': health, 'launch_trace': zero_trace(run_id, before)})


@redacted
def pinned(run_id, account, role, context_id):
    socket, ids, health, compositor = prepared(run_id, account, role, context_id, 'pinned')
    before = desktop_boot.trace_snapshot(run_id)
    close_anchored(account, socket, context_id)
    absent(run_id, account, socket, context_id, 'pinned', compositor, health['daemon_pid'])
    new_pid = restart_daemon(run_id, account, socket, health['daemon_pid'])
    restart_ms, restart_count, _health = absent(run_id, account, socket, context_id, 'pinned', compositor, new_pid)
    guest.sway_command(account, socket, 'reload')
    reload_ms, reload_count, _health = absent(run_id, account, socket, context_id, 'pinned', compositor, new_pid)
    after = compositor_pid(account, guest.guest_socket(account))
    require(after == compositor)
    return finish('pinned', role, ids, {'context_id': context_id, 'context_state': 'active', 'policy': 'pinned',
        'present_before': True, 'fresh_close_event': True, 'desired_open': True,
        'production_close_grace_ms': GRACE_MS, 'steady_samples': min(restart_count, reload_count),
        'daemon_restart': True, 'sway_reload': True, 'compositor_before': compositor, 'compositor_after': after,
        'absent_after_restart': True, 'absent_after_reload': True, 'restart_absence_ms': restart_ms,
        'reload_absence_ms': reload_ms, 'launch_trace': zero_trace(run_id, before)})
