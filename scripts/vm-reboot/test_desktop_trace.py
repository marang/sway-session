"""Pinned launch-route and source checks; no VM, app, tracefs or network use."""
import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import desktop_trace as trace

BOOT = 'fa074a61-5fb7-4899-9739-f93df26a5ff7'
OTHER_BOOT = '41363ed9-35cd-47b6-b6a3-375b0fc90736'


def proof():
    return dict(trace.SOURCE_PINS, portal_sha256='e' * 64)


def fork(parent, child):
    return f'private-1 [000] .... 1.0: sched_process_fork: comm=private pid={parent} child_comm=private child_pid={child}'


def execute(pid, filename, old=None):
    return f'private-1 [000] .... 1.1: sched_process_exec: filename={filename} pid={pid} old_pid={pid if old is None else old}'


def exit_task(pid):
    return f'private-1 [000] .... 1.2: sched_process_exit: comm=private pid={pid} prio=120 group_dead=true'


def main_route(pid, parent=1):
    return [fork(parent, pid), execute(pid, '/app/bin/com.slack.Slack'),
            execute(pid, '/usr/bin/env'), execute(pid, '/app/bin/zypak-wrapper'),
            execute(pid, '/app/bin/zypak-helper'), execute(pid, '/app/extra/slack')]


def child_route(pid, portal=50):
    return [fork(portal, pid), execute(pid, '/usr/bin/flatpak'), execute(pid, '/usr/bin/bwrap'),
            execute(pid, '/app/bin/zypak-helper'), execute(pid, '/app/extra/slack')]


def consume(record, lines):
    for line in lines:
        record.consume(line)


class LaunchRoutes(unittest.TestCase):
    def record(self):
        return trace.Incarnations(proof())

    def test_one_normal_main_and_independent_portal_child_are_distinguished(self):
        record = self.record()
        consume(record, main_route(10))
        consume(record, [execute(50, '/usr/lib/flatpak-portal'), *child_route(60)])
        self.assertEqual(record.starts, {'chrome': 0, 'slack': 1})
        self.assertEqual(record.helpers, {'slack': 1})
        self.assertEqual(record.unclassified, 0)
        self.assertNotIn('private', str(record.state(BOOT)))

    def test_second_short_main_is_counted_even_with_one_surviving_window(self):
        record = self.record()
        consume(record, main_route(10) + main_route(20) + [exit_task(20)])
        self.assertEqual(record.starts['slack'], 2)
        self.assertNotIn(20, record.processes)

    def test_main_fork_and_same_incarnation_reexec_do_not_add_starts(self):
        record = self.record()
        consume(record, main_route(10) + [fork(10, 11), execute(11, '/app/extra/slack'),
                execute(10, '/app/bin/zypak-helper'), execute(10, '/app/extra/slack')])
        self.assertEqual(record.starts['slack'], 1)
        self.assertEqual(record.helpers['slack'], 0)

    def test_helper_reexec_and_descendants_are_transparent_without_extra_mains(self):
        record = self.record()
        consume(record, [execute(50, '/usr/lib/flatpak-portal'), *child_route(60),
                        execute(60, '/app/extra/slack'), fork(60, 61), execute(61, '/app/extra/slack')])
        self.assertEqual(record.starts['slack'], 0)
        self.assertEqual(record.helpers['slack'], 3)

    def test_unknown_direct_or_helper_only_origins_fail_closed(self):
        for lines in [[execute(10, '/app/extra/slack')],
                      [execute(10, '/app/bin/zypak-helper'), execute(10, '/app/extra/slack')],
                      [execute(50, '/tmp/flatpak-portal'), *child_route(60)],
                      [execute(10, '/app/bin/zypak-wrapper'), execute(10, '/app/bin/zypak-helper'), execute(10, '/app/extra/slack')]]:
            record = self.record()
            with self.subTest(lines=lines), self.assertRaisesRegex(RuntimeError, 'origin_unclassified'):
                consume(record, lines)
            self.assertEqual(record.unclassified, 1)
            with self.assertRaises(RuntimeError):
                record.state(BOOT)

    def test_normal_wrapper_wins_when_main_launch_also_comes_through_portal(self):
        record = self.record()
        consume(record, [execute(50, '/usr/lib/flatpak-portal'), *main_route(10, parent=50)])
        self.assertEqual(record.starts['slack'], 1)
        self.assertEqual(record.helpers['slack'], 0)

    def test_stage_order_and_fresh_entry_reset_prevent_stale_helper_evidence(self):
        record = self.record()
        with self.assertRaises(RuntimeError):
            consume(record, [execute(10, '/app/bin/zypak-helper'), execute(10, '/app/bin/com.slack.Slack'),
                             execute(10, '/app/bin/zypak-wrapper'), execute(10, '/app/extra/slack')])

    def test_pid_lifetime_preserves_existing_children_and_does_not_trust_reuse(self):
        record = self.record()
        consume(record, [execute(50, '/usr/lib/flatpak-portal'), fork(50, 60), exit_task(50),
                        execute(60, '/app/bin/zypak-helper'), execute(60, '/app/extra/slack')])
        self.assertEqual(record.helpers['slack'], 1)
        with self.assertRaises(RuntimeError):
            consume(record, [fork(1, 50), execute(50, '/app/bin/zypak-helper'), execute(50, '/app/extra/slack')])

    def test_old_pid_leader_transfer_preserves_all_main_and_helper_provenance(self):
        record = self.record()
        consume(record, main_route(10) + [fork(10, 11), execute(10, '/app/extra/slack', old=11)])
        self.assertEqual(record.starts['slack'], 1)
        self.assertNotIn(11, record.processes)
        consume(record, [execute(50, '/usr/lib/flatpak-portal'), fork(50, 60), fork(60, 61),
                        execute(60, '/app/bin/zypak-helper', old=61), execute(60, '/app/extra/slack')])
        self.assertEqual(record.helpers['slack'], 1)
        self.assertNotIn(61, record.processes)

    def test_chrome_and_fixed_callback_ancestry_remain_independent(self):
        record = self.record()
        consume(record, [fork(1, 10), execute(10, '/opt/google/chrome/chrome'),
                        fork(10, 11), execute(11, '/opt/google/chrome/chrome'),
                        fork(1, 20), execute(20, '/usr/bin/sway-session'),
                        fork(1, 30), execute(30, '/usr/bin/swaynag'), fork(30, 31),
                        execute(31, '/bin/sh'), execute(31, '/usr/bin/sway-session')])
        self.assertEqual(record.starts, {'chrome': 1, 'slack': 0})
        self.assertEqual(record.callbacks, 1)

    def test_state_needs_explicit_source_proof_and_copies_mutable_counts(self):
        with self.assertRaises(RuntimeError):
            trace.Incarnations().state(BOOT)
        record = self.record()
        value = record.state(BOOT)
        value['starts']['slack'] = 99
        value['helpers']['slack'] = 99
        value['sources']['helper_sha256'] = '0' * 64
        self.assertEqual(record.state(BOOT)['starts']['slack'], 0)
        self.assertEqual(record.state(BOOT)['helpers']['slack'], 0)
        self.assertEqual(record.state(BOOT)['sources'], proof())

    def test_delta_preserves_classification_source_and_independent_helper_counts(self):
        record = self.record()
        before = record.state(BOOT)
        consume(record, main_route(10) + [execute(50, '/usr/lib/flatpak-portal'), *child_route(60)])
        after = record.state(BOOT)
        result = trace.delta(before, after)
        self.assertEqual(result, {'complete': True, 'measurement': 'sched_process_exec_fork_exit',
            'starts': {'chrome': 0, 'slack': 1}, 'classification': 'fixed-normal-launchers-v1',
            'helpers': {'slack': 1}, 'unclassified': 0, 'sources': proof()})
        for key, replacement in [('boot_id', OTHER_BOOT), ('classification', 'heuristic'),
                                 ('unclassified', 1), ('events', -1)]:
            value = dict(after, **{key: replacement})
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                trace.delta(before, value)
        for mutate in [lambda v: v['sources'].update(helper_sha256='0' * 64),
                       lambda v: v['starts'].update(slack=True),
                       lambda v: v['helpers'].update(slack=-1)]:
            value = copy.deepcopy(after)
            mutate(value)
            with self.assertRaises(RuntimeError):
                trace.delta(before, value)

    def test_unknown_loss_and_event_process_budgets_fail_closed(self):
        for line in ['CPU:0 [LOST 1 EVENTS]', 'task: sys_enter_execve: argv=private',
                     'task: sched_process_exec: malformed']:
            with self.assertRaises(RuntimeError):
                self.record().consume(line)
        with patch.object(trace, 'MAX_EVENTS', 1):
            record = self.record()
            record.consume('')
            with self.assertRaises(RuntimeError):
                record.consume('')
        with patch.object(trace, 'MAX_PROCESSES', 1):
            record = self.record()
            record.consume(fork(1, 2))
            with self.assertRaises(RuntimeError):
                record.consume(fork(1, 3))


class PinnedSources(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='lab253-trace-source-test-')
        self.addCleanup(self.temp.cleanup)
        self.files = Path(self.temp.name)
        (self.files / 'bin').mkdir()
        self.pins = dict(trace.SOURCE_PINS)
        for relative, key, data in [
                ('manifest.json', 'manifest_sha256', b'public synthetic package manifest'),
                ('bin/com.slack.Slack', 'entry_sha256', b'public synthetic installed app wrapper'),
                ('bin/zypak-wrapper.sh', 'wrapper_sha256', b'public synthetic zypak wrapper'),
                ('bin/zypak-helper', 'helper_sha256', b'public synthetic helper ELF')]:
            path = self.files / relative
            path.write_bytes(data)
            path.chmod(0o644 if relative == 'manifest.json' else 0o755)
            self.pins[key] = hashlib.sha256(data).hexdigest()
        (self.files / 'bin/zypak-wrapper').symlink_to('/app/bin/zypak-wrapper.sh')
        patcher = patch.object(trace, 'SOURCE_PINS', self.pins)
        patcher.start()
        self.addCleanup(patcher.stop)
        actual_digest = trace.source_digest

        def digest(path, *, root_owned=False, executable=False):
            if path == Path('/usr/lib/flatpak-portal'):
                self.assertTrue(root_owned)
                self.assertTrue(executable)
                return 'e' * 64
            return actual_digest(path, root_owned=root_owned, executable=executable)

        patcher = patch.object(trace, 'source_digest', digest)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_code_pins_and_expected_alias_are_verified_without_launches(self):
        self.assertEqual(trace.validate_sources(self.files), dict(self.pins, portal_sha256='e' * 64))
        (self.files / 'bin/zypak-wrapper.sh').write_bytes(b'changed public code')
        with self.assertRaisesRegex(RuntimeError, 'digest_changed'):
            trace.validate_sources(self.files)

    def test_every_packaged_source_and_helper_binary_must_match(self):
        for relative in ['manifest.json', 'bin/com.slack.Slack', 'bin/zypak-helper']:
            path = self.files / relative
            original = path.read_bytes()
            path.write_bytes(b'changed')
            with self.subTest(relative=relative), self.assertRaises(RuntimeError):
                trace.validate_sources(self.files)
            path.write_bytes(original)

    def test_alias_regular_file_symlink_and_mode_changes_are_rejected(self):
        alias = self.files / 'bin/zypak-wrapper'
        alias.unlink()
        alias.symlink_to('/tmp/untrusted-wrapper')
        with self.assertRaises(RuntimeError):
            trace.validate_sources(self.files)
        alias.unlink()
        alias.symlink_to('/app/bin/zypak-wrapper.sh')
        helper = self.files / 'bin/zypak-helper'
        helper.chmod(0o666)
        with self.assertRaises(RuntimeError):
            trace.validate_sources(self.files)
        helper.unlink()
        helper.symlink_to('/tmp/untrusted-helper')
        with self.assertRaises(RuntimeError) as caught:
            trace.validate_sources(self.files)
        self.assertNotIn(str(self.files), str(caught.exception))

    def test_source_proof_is_strict_and_saved_proof_revalidates_deployed_bytes(self):
        sources = trace.validate_sources(self.files)
        for key, replacement in [('version', True), ('slack_commit', '0' * 64),
                                 ('zypak_source_commit', '0' * 40), ('portal_sha256', 'invalid')]:
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                trace.source_proof(dict(sources, **{key: replacement}))
        with self.assertRaises(RuntimeError):
            trace.source_proof(dict(sources, arbitrary_error='private'))
        target = self.files / 'proof.json'
        target.write_text(json.dumps(sources))
        with patch.object(trace, 'SOURCE_FILE', target), \
                patch.object(trace.guest, 'private_artifact'), \
                patch.object(trace, 'deployment_files', return_value=self.files):
            self.assertEqual(trace.load_sources(), sources)
            (self.files / 'manifest.json').write_text('changed public manifest')
            with self.assertRaises(RuntimeError):
                trace.load_sources()

    def test_preexisting_portal_is_rejected_and_other_helpers_are_not_guessed(self):
        with patch.object(Path, 'iterdir', return_value=iter([Path('/proc/77')])), \
                patch.object(os, 'readlink', return_value='/usr/lib/flatpak-portal'):
            with self.assertRaisesRegex(RuntimeError, 'predates_recorder'):
                trace.require_no_portal()
        with patch.object(Path, 'iterdir', return_value=iter([Path('/proc/77')])), \
                patch.object(os, 'readlink', return_value='/usr/lib/flatpak-session-helper'):
            trace.require_no_portal()

    def test_each_loss_counter_and_missing_cpu_statistics_rejects(self):
        root = self.files / 'trace-instance'
        root.mkdir()
        with self.assertRaises(RuntimeError):
            trace.overrun(root)
        path = root / 'per_cpu/cpu0/stats'
        path.parent.mkdir(parents=True)
        keys = ['overrun', 'commit overrun', 'dropped events']
        for nonzero in keys:
            path.write_text('\n'.join(f'{key}: {int(key == nonzero)}' for key in keys))
            with self.subTest(nonzero=nonzero), self.assertRaises(RuntimeError):
                trace.overrun(root)
        path.write_text('\n'.join(f'{key}: 0' for key in keys))
        trace.overrun(root)


if __name__ == '__main__':
    unittest.main()
