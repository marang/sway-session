import importlib.util
import sys
from pathlib import Path
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import desktop_guest as desktop
import desktop_trace as trace


def fork(parent, child):
    return f'private-1 [000] .... 1.0: sched_process_fork: comm=private pid={parent} child_comm=private child_pid={child}'


def execute(pid, path, old=None):
    return f'private-1 [000] .... 1.1: sched_process_exec: filename={path} pid={pid} old_pid={pid if old is None else old}'


def exit_process(pid):
    return f'private-1 [000] .... 1.2: sched_process_exit: comm=private pid={pid} prio=120'


class ProcessTraceTests(unittest.TestCase):
    def test_independent_short_root_counts_while_helpers_and_reexec_do_not(self):
        record = trace.Incarnations()
        for line in [fork(1, 20), execute(20, '/opt/google/chrome/chrome'),
                     fork(20, 21), execute(21, '/opt/google/chrome/chrome'),
                     execute(20, '/opt/google/chrome/chrome'),
                     fork(1, 30), execute(30, '/opt/google/chrome/chrome'), exit_process(30)]:
            record.consume(line)
        self.assertEqual(record.starts, {'chrome': 2, 'slack': 0})
        self.assertNotIn('private', str(record.state('boot')))

    def test_dead_parent_descendant_keeps_incarnation_and_recycled_pid_is_fresh(self):
        record = trace.Incarnations()
        for line in [fork(1, 20), execute(20, '/app/extra/slack/slack'), fork(20, 21),
                     exit_process(20), execute(21, '/app/extra/slack/slack'),
                     fork(1, 20), execute(20, '/app/extra/slack/slack')]:
            record.consume(line)
        self.assertEqual(record.starts['slack'], 2)

    def test_exec_thread_leader_remapping_does_not_double_count(self):
        record = trace.Incarnations()
        for line in [fork(1, 20), execute(20, '/opt/google/chrome/chrome'), fork(20, 21),
                     execute(20, '/opt/google/chrome/chrome', old=21)]:
            record.consume(line)
        self.assertEqual(record.starts['chrome'], 1)
        self.assertNotIn(21, record.processes)

    def test_callback_requires_swaynag_process_ancestry(self):
        record = trace.Incarnations()
        for line in [fork(1, 10), execute(10, '/usr/bin/sway-session'),
                     fork(1, 20), execute(20, '/usr/bin/swaynag'), fork(20, 21),
                     execute(21, '/bin/sh'), fork(21, 22), execute(22, '/usr/bin/sway-session')]:
            record.consume(line)
        self.assertEqual(record.callbacks, 1)

    def test_unknown_events_and_malformed_exec_fail_closed(self):
        for line in ['task: sched_process_exec: unexpected', 'task: sys_enter_execve: argv=private']:
            with self.subTest(line=line), self.assertRaises(RuntimeError):
                trace.Incarnations().consume(line)

    def test_trace_scope_cannot_cross_boot_or_regress(self):
        before = {'boot_id': 'a', 'events': 10, 'starts': {'chrome': 1, 'slack': 0}}
        after = {'boot_id': 'b', 'events': 20, 'starts': {'chrome': 1, 'slack': 0}}
        with self.assertRaises(RuntimeError):
            trace.delta(before, after)
        after['boot_id'], after['starts']['chrome'] = 'a', 0
        with self.assertRaises(RuntimeError):
            trace.delta(before, after)


class GUIControllerTests(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
