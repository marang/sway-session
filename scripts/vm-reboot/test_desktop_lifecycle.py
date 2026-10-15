"""Bounded IPC and lifecycle assertions using only synthetic guest seams."""
import json
from pathlib import Path
import struct
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import desktop_lifecycle as life
from test_desktop_boot import ARTIFACT, IDS, RUN, harness, report, snapshot


def frame(kind, value):
    payload = json.dumps(value).encode()
    return b'i3-ipc' + struct.pack('<II', len(payload), kind) + payload


class Peer:
    def __init__(self, data, chunk=3):
        self.data, self.chunk, self.closed = data, chunk, False
        self.sent = b''
    def recv(self, count):
        value, self.data = self.data[:min(count, self.chunk)], self.data[min(count, self.chunk):]
        return value
    def settimeout(self, value): self.timeout = value
    def connect(self, path): self.path = path
    def sendall(self, value): self.sent += value
    def close(self): self.closed = True


def probe(*, present=True, policy='follow', desired=True, focused=True):
    anchor = {'container_id': 98, 'workspace': '98'}
    return {'scenario': 'desktop_probe', 'focused': dict(anchor, context_id=IDS[0]) if focused else None,
        'groups': [{'context_id': IDS[0], 'state': 'active', 'policy': policy, 'desired_open': desired,
            'presence': 'present' if present else 'absent', 'window_count': int(present),
            'anchor_marked': present, 'anchor': anchor if present else None,
            'windows': [anchor] if present else []}]}


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.account = SimpleNamespace(pw_uid=1000)
        self.socket = Path('/run/user/1000/sway-ipc.1000.100.sock')
        self.patch(life.guest, 'guard', return_value=(RUN, self.account))
        self.patch(life.desktop_boot, 'guarded', return_value=self.account)
        self.patch(life.desktop_boot, 'registered_ids', return_value=IDS.copy())
        self.patch(life.guest, 'guest_socket', return_value=self.socket)
        self.patch(life.guest, 'artifact_identity', return_value=ARTIFACT)
        self.patch(life.guest, 'load_harness', return_value=harness())
        self.patch(life.guest, 'health', return_value={key: report()[key] for key in life.desktop_evidence.HEALTH})
        self.patch(life.desktop_guest, 'probe', return_value=probe())

    def patch(self, target, attribute, *args, **kwargs):
        item = patch.object(target, attribute, *args, **kwargs)
        result = item.start()
        self.addCleanup(item.stop)
        return result

    def test_fragmented_frames_subscribe_and_exact_fresh_close_attribution(self):
        data = frame(2, {'success': True})
        for change, identity in [('focus', 98), ('close', 99), ('close', 98)]:
            data += frame(life.WINDOW_EVENT, {'change': change, 'container': {'id': identity, 'name': 'PRIVATE'}})
        peer = Peer(data)
        self.patch(life.sockets, 'socket', return_value=peer)
        with life.WindowEvents(self.socket) as events:
            self.assertTrue(events.closed(98))
        self.assertTrue(peer.closed)
        self.assertEqual(peer.sent, b'i3-ipc' + struct.pack('<II', 10, 2) + b'["window"]')

    def test_invalid_magic_size_truncation_ack_and_unknown_event_fail(self):
        variants = [b'NOTIPC' + struct.pack('<II', 2, 2) + b'{}',
            b'i3-ipc' + struct.pack('<II', life.FRAME_LIMIT + 1, 2),
            b'i3-ipc' + struct.pack('<II', 0, 2), b'i3-ipc',
            frame(2, {'success': False}), frame(2, {'success': 1}), frame(3, {'success': True}),
            frame(2, {'success': True}) + frame(99, {'change': 'close', 'container': {'id': 98}})]
        for data in variants:
            with self.subTest(data=data):
                peer = Peer(data)
                self.patch(life.sockets, 'socket', return_value=peer)
                with self.assertRaises((RuntimeError, ValueError)):
                    with life.WindowEvents(self.socket) as events: events.closed(98)
                self.assertTrue(peer.closed)

    def test_missing_close_event_exhaustion_and_deadline_are_bounded(self):
        peer = Peer(frame(2, {'success': True}) + frame(life.WINDOW_EVENT, {'change': 'focus'}) * 3)
        self.patch(life.sockets, 'socket', return_value=peer)
        self.patch(life, 'MAX_EVENTS', 3)
        with self.assertRaises(RuntimeError):
            with life.WindowEvents(self.socket) as events: events.closed(98)
        events = life.WindowEvents(self.socket)
        events.peer = Peer(b'anything')
        self.patch(life.time, 'monotonic', return_value=11)
        with self.assertRaises(RuntimeError): events.exact(1, 10)

    def test_close_subscribes_before_focusing_revalidating_and_killing_exact_anchor(self):
        order = []
        class Events:
            def __init__(self, path): self.path = path
            def __enter__(self): order.append('subscribe'); return self
            def __exit__(self, *_args): order.append('unsubscribe')
            def closed(self, value): order.append(('close event', value)); return True
        self.patch(life, 'WindowEvents', Events)
        command = self.patch(life.guest, 'sway_command', side_effect=lambda _a, _s, text: order.append(text))
        self.assertEqual(life.close_anchored(self.account, self.socket, IDS[0]), {'container_id': 98, 'fresh_close_event': True})
        self.assertEqual(order, ['subscribe', '[con_id=98] focus', '[con_id=98] kill', ('close event', 98), 'unsubscribe'])
        self.assertEqual(command.call_count, 2)

    def test_close_rejects_bad_focus_ambiguity_workspace_and_wrong_context(self):
        for change in ['focus', 'context', 'workspace', 'ambiguous', 'count', 'unmarked', 'anchor swap']:
            with self.subTest(change=change):
                value = probe()
                if change == 'focus': value['focused'] = None
                if change == 'context': value['focused']['context_id'] = IDS[1]
                if change == 'workspace': value['groups'][0]['anchor']['workspace'] = '1'
                if change == 'ambiguous': value['groups'][0]['presence'] = 'ambiguous'
                if change == 'count': value['groups'][0]['window_count'] = 2
                if change == 'unmarked': value['groups'][0]['anchor_marked'] = False
                if change == 'anchor swap': value['groups'][0]['anchor']['container_id'] = 99
                self.patch(life.desktop_guest, 'probe', return_value=value)
                command = self.patch(life.guest, 'sway_command')
                self.patch(life, 'WindowEvents', return_value=Mock(__enter__=Mock(return_value=Mock()), __exit__=Mock(return_value=False)))
                with self.assertRaisesRegex(RuntimeError, life.FAILURE): life.close_anchored(self.account, self.socket, IDS[0])
                self.assertFalse(any('kill' in str(call) for call in command.call_args_list))

    def test_missing_exact_close_event_never_claims_success(self):
        events = Mock()
        events.closed.return_value = False
        self.patch(life, 'WindowEvents', return_value=Mock(__enter__=Mock(return_value=events), __exit__=Mock(return_value=False)))
        self.patch(life.guest, 'sway_command')
        with self.assertRaisesRegex(RuntimeError, life.FAILURE): life.close_anchored(self.account, self.socket, IDS[0])

    def test_health_rejects_wrong_inhibitor_uid_binary_pid_or_artifact(self):
        for change in ['uid', 'inhibitor pid', 'binary', 'doctor', 'artifact']:
            with self.subTest(change=change):
                value = {key: report()[key] for key in life.desktop_evidence.HEALTH}
                if change == 'uid': value['inhibitor']['uid'] = 1001
                if change == 'inhibitor pid': value['inhibitor']['pid'] = 701
                if change == 'binary': value['daemon_binary_current'] = False
                if change == 'doctor': value['doctor'][1]['status'] = 'warning'
                self.patch(life.guest, 'health', return_value=value)
                artifact = self.patch(life.guest, 'artifact_identity', return_value=ARTIFACT if change != 'artifact' else dict(ARTIFACT, sha256='e' * 64))
                with self.assertRaises(RuntimeError): life.healthy(RUN, self.account, self.socket)

    def test_preconditions_require_correct_role_policy_and_live_anchor(self):
        self.patch(life, 'compositor_pid', return_value=100)
        self.assertEqual(life.prepared(RUN, self.account, 'chrome', IDS[0], 'follow')[3], 100)
        for role, identity, policy in [('slack', IDS[0], 'follow'), ('other', IDS[0], 'follow'), ('chrome', IDS[0], 'pinned')]:
            with self.assertRaises(RuntimeError): life.prepared(RUN, self.account, role, identity, policy)
        self.patch(life.desktop_guest, 'probe', return_value=probe(present=False))
        with self.assertRaises(RuntimeError): life.prepared(RUN, self.account, 'chrome', IDS[0], 'follow')

    def test_compositor_pid_uses_only_owned_canonical_socket_and_executable(self):
        owned = self.patch(life, 'owned_executable')
        self.assertEqual(life.compositor_pid(self.account, self.socket), 100)
        owned.assert_called_once_with(self.account, 100, Path('/usr/bin/sway'))
        for invalid in ['/tmp/sway-ipc.1000.100.sock', '/run/user/1000/sway-ipc.0.100.sock', '/run/user/1000/sway-ipc.1000.0.sock']:
            with self.assertRaises(RuntimeError): life.compositor_pid(self.account, Path(invalid))

    def absence_seams(self, value):
        self.patch(life.desktop_guest, 'probe', return_value=value)
        self.patch(life, 'compositor_pid', return_value=100)
        self.patch(life.time, 'sleep')
        self.patch(life.time, 'monotonic', side_effect=(index * .25 for index in range(1000)))

    def test_absence_requires_real_elapsed_grace_three_samples_and_correct_intent(self):
        self.absence_seams(probe(present=False, desired=False))
        elapsed, count, health = life.absent(RUN, self.account, self.socket, IDS[0], 'follow', 100, 700)
        self.assertGreater(elapsed, 2000)
        self.assertGreaterEqual(count, 3)
        self.assertEqual(health['daemon_pid'], 700)

    def test_reappearance_wrong_policy_daemon_or_compositor_fails_absence(self):
        for change in ['reappear', 'policy', 'daemon', 'compositor', 'intent']:
            with self.subTest(change=change):
                self.absence_seams(probe(present=False, desired=False))
                if change == 'reappear': self.patch(life.desktop_guest, 'probe', side_effect=[probe(present=False, desired=False), probe()])
                if change == 'policy': self.patch(life.desktop_guest, 'probe', return_value=probe(present=False, policy='pinned'))
                if change == 'daemon': self.patch(life, 'healthy', return_value=dict(report(), daemon_pid=701))
                if change == 'compositor': self.patch(life, 'compositor_pid', return_value=101)
                if change == 'intent': self.patch(life.desktop_guest, 'probe', return_value=probe(present=False, desired=True))
                with self.assertRaises(RuntimeError): life.absent(RUN, self.account, self.socket, IDS[0], 'follow', 100, 700)

    def test_daemon_restart_signals_only_attributed_pidfd_and_uses_original_sway_session(self):
        owned = self.patch(life, 'owned_executable')
        opener = self.patch(life.os, 'pidfd_open', return_value=44)
        sender = self.patch(life.signal, 'pidfd_send_signal')
        self.patch(life.select, 'select', return_value=([44], [], []))
        close = self.patch(life.os, 'close')
        health = self.patch(life, 'healthy', side_effect=[{'daemon_pid': 700}, {'daemon_pid': 701}])
        command = self.patch(life.guest, 'sway_command')
        self.assertEqual(life.restart_daemon(RUN, self.account, self.socket, 700), 701)
        opener.assert_called_once_with(700, 0)
        sender.assert_called_once_with(44, life.signal.SIGTERM)
        owned.assert_called_once_with(self.account, 700, Path('/usr/bin/sway-session'))
        command.assert_called_once_with(self.account, self.socket, 'exec --no-startup-id /usr/bin/sway-session daemon')
        close.assert_called_once_with(44)

    def test_daemon_stop_timeout_or_attribution_change_never_starts_replacement(self):
        self.patch(life, 'owned_executable')
        self.patch(life.os, 'pidfd_open', return_value=44)
        self.patch(life.signal, 'pidfd_send_signal')
        self.patch(life.select, 'select', return_value=([], [], []))
        close = self.patch(life.os, 'close')
        self.patch(life, 'healthy', return_value={'daemon_pid': 700})
        command = self.patch(life.guest, 'sway_command')
        with self.assertRaises(RuntimeError): life.restart_daemon(RUN, self.account, self.socket, 700)
        command.assert_not_called()
        close.assert_called_once_with(44)

    def test_daemon_pid_or_executable_change_prevents_any_signal(self):
        self.patch(life.os, 'pidfd_open', return_value=44)
        sender = self.patch(life.signal, 'pidfd_send_signal')
        self.patch(life.os, 'close')
        self.patch(life, 'healthy', return_value={'daemon_pid': 701})
        command = self.patch(life.guest, 'sway_command')
        with self.assertRaises(RuntimeError): life.restart_daemon(RUN, self.account, self.socket, 700)
        sender.assert_not_called()
        command.assert_not_called()
        self.patch(life, 'healthy', return_value={'daemon_pid': 700})
        self.patch(life, 'owned_executable', side_effect=RuntimeError('wrong owner or executable'))
        with self.assertRaises(RuntimeError): life.restart_daemon(RUN, self.account, self.socket, 700)
        sender.assert_not_called()
        command.assert_not_called()

    def phase_seams(self, policy):
        health = {key: report()[key] for key in life.desktop_evidence.HEALTH}
        self.patch(life, 'prepared', return_value=(self.socket, IDS, health, 100))
        self.patch(life.desktop_boot, 'trace_snapshot', side_effect=[snapshot(), snapshot(5, events=200)])
        self.close = self.patch(life, 'close_anchored', return_value={'container_id': 98, 'fresh_close_event': True})
        self.absent = self.patch(life, 'absent', return_value=(2501, 3, health))
        self.command = self.patch(life.guest, 'sway_command')
        self.patch(life, 'compositor_pid', return_value=100)
        self.patch(life, 'restart_daemon', return_value=701)

    def test_follow_returns_checked_facts_only_after_fresh_event_absence_and_zero_starts(self):
        self.phase_seams('follow')
        value = life.follow(RUN, self.account, 'chrome', IDS[0])
        self.assertFalse(value['desired_open'])
        self.assertEqual(value['absence_ms'], 2501)
        self.assertEqual(value['launch_trace']['starts'], {'chrome': 0, 'slack': 0})
        self.close.assert_called_once_with(self.account, self.socket, IDS[0])
        self.command.assert_not_called()

    def test_pinned_observes_absence_after_close_restart_and_reload_without_restore(self):
        self.phase_seams('pinned')
        value = life.pinned(RUN, self.account, 'chrome', IDS[0])
        self.assertTrue(value['desired_open'])
        self.assertEqual(value['restart_absence_ms'], 2501)
        self.assertEqual(value['reload_absence_ms'], 2501)
        self.assertEqual(self.absent.call_count, 3)
        self.command.assert_called_once_with(self.account, self.socket, 'reload')
        self.assertEqual(value['compositor_before'], value['compositor_after'])

    def test_pinned_cannot_claim_success_after_compositor_replacement(self):
        self.phase_seams('pinned')
        self.patch(life, 'compositor_pid', return_value=101)
        with self.assertRaisesRegex(RuntimeError, life.FAILURE): life.pinned(RUN, self.account, 'chrome', IDS[0])

    def test_duplicate_launch_or_trace_failure_never_returns_passing_facts(self):
        self.phase_seams('follow')
        self.patch(life.desktop_boot, 'trace_snapshot', side_effect=[snapshot(), snapshot(6, events=200)])
        with self.assertRaisesRegex(RuntimeError, life.FAILURE): life.follow(RUN, self.account, 'chrome', IDS[0])
        self.patch(life.desktop_boot, 'trace_snapshot', side_effect=RuntimeError('PRIVATE TRACE CONTENT'))
        with self.assertRaisesRegex(RuntimeError, '^' + life.FAILURE + '$'): life.follow(RUN, self.account, 'chrome', IDS[0])

    def test_public_helpers_redact_raw_guard_and_health_errors(self):
        self.patch(life.guest, 'guard', side_effect=RuntimeError('PRIVATE SOCKET ENV TITLE'))
        self.patch(life.desktop_boot, 'guarded', side_effect=RuntimeError('PRIVATE GUARD'))
        for operation in [lambda: life.close_anchored(self.account, self.socket, IDS[0]),
                          lambda: life.follow(RUN, self.account, 'chrome', IDS[0]),
                          lambda: life.pinned(RUN, self.account, 'chrome', IDS[0])]:
            with self.assertRaisesRegex(RuntimeError, '^' + life.FAILURE + '$'): operation()


if __name__ == '__main__':
    unittest.main()
