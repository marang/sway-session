"""Validate narrow guest CLI/Doctor/logind reports with synthetic observations."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('guest_fixture', Path(__file__).with_name('guest.py'))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)

class GuestReports(unittest.TestCase):
    def test_desktop_pam_profile_supplies_actual_session_type_before_sway(self):
        with tempfile.TemporaryDirectory() as directory:
            profile = Path(directory) / 'profile'
            for desktop in [False, True]:
                profile.write_text(guest.login_profile(desktop=desktop))
                # No tty is attached, so this sources only the exports and never
                # launches Sway. Inspect only the two explicit public settings.
                result = subprocess.run(['/usr/bin/bash', '--noprofile', '--norc', '-c',
                    '. "$1"; printf "%s,%s" "${XDG_CURRENT_DESKTOP-}" "${XDG_SESSION_TYPE-}"',
                    'fixture-profile', str(profile)], env={'HOME': directory}, stdin=subprocess.DEVNULL,
                    capture_output=True, check=True, timeout=5)
                self.assertEqual(result.stdout.decode(), 'sway,wayland' if desktop else ',')

    def test_non_kvm_refusal_precedes_any_fixture_mutation(self):
        calls = []
        def probe(argv, **kwargs):
            calls.append(argv)
            self.assertEqual(argv, ['systemd-detect-virt', '--vm'])
            return 'qemu\n'
        with patch.object(guest.os, 'geteuid', return_value=0), patch.object(guest, 'command', probe):
            with self.assertRaisesRegex(RuntimeError, 'KVM'):
                guest.main(['install'])
        self.assertEqual(len(calls), 1)

    def test_terminal_creation_uses_the_established_actions_field(self):
        value = '6ba7b810-9dad-41d1-80b4-00c04fd430c8'
        report = {'command':'terminal', 'terminal':{'context_id':value,'actions':['created','attached','focused']}, 'contexts':[{'id':value}]}
        self.assertEqual(guest.terminal_id(report, []), value)
        for change in ['reused', 'duplicate', 'wrong context']:
            altered, existing = copy.deepcopy(report), []
            if change == 'reused': altered['terminal']['actions'] = ['attached','focused']
            if change == 'duplicate': existing = [value]
            if change == 'wrong context': altered['contexts'] = []
            with self.assertRaises(RuntimeError):
                guest.terminal_id(altered, existing)

    def test_doctor_requires_matching_daemon_pid_and_binary(self):
        report = {'doctor':{'checks':[
            {'id':'daemon.lock','status':'ok','evidence':['pid=123']},
            {'id':'daemon.binary','status':'ok','evidence':['pid=123','path=/usr/local/bin/sway-session','inode match']},
            {'id':'other','status':'warning','evidence':['PRIVATE TITLE']} ]}}
        self.assertEqual(guest.doctor_evidence(report), (123,[{'id':'daemon.lock','status':'ok'},{'id':'daemon.binary','status':'ok'}]))
        for change in ['warning','pid','missing','duplicate']:
            altered = copy.deepcopy(report)
            rows = altered['doctor']['checks']
            if change == 'warning': rows[1]['status'] = 'warning'
            if change == 'pid': rows[1]['evidence'][0] = 'pid=456'
            if change == 'missing': rows.pop(0)
            if change == 'duplicate': rows.append(rows[0])
            with self.assertRaises(RuntimeError):
                guest.doctor_evidence(altered)

    def test_desktop_doctor_must_verify_fixed_approval_callback_candidate(self):
        report = {'doctor': {'checks': [
            {'id': 'daemon.lock', 'status': 'ok', 'evidence': ['pid=123']},
            {'id': 'daemon.binary', 'status': 'ok',
             'evidence': ['pid=123', 'path=/usr/bin/sway-session', 'digest match']} ]}}
        self.assertEqual(guest.doctor_evidence(report, candidate=Path('/usr/bin/sway-session'))[0], 123)
        with self.assertRaises(RuntimeError):
            guest.doctor_evidence(report)
        for destination in ['/tmp/sway-session', '/usr/bin/../bin/sway-session', '/usr/bin/other']:
            with self.assertRaises(RuntimeError):
                guest.doctor_evidence(report, candidate=Path(destination))

    def test_desktop_session_routing_loads_actual_bytes_and_rejects_stale_boot(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            routing = home / '.local/state/fixture-session-env.json'
            routing.parent.mkdir(parents=True)
            socket = Path('/run/user') / str(os.getuid()) / 'sway-ipc.fixture.sock'
            bus = Path('/tmp/dbus-lab253fixture')
            account = SimpleNamespace(pw_uid=os.getuid())
            environment = {'DBUS_SESSION_BUS_ADDRESS': 'unix:path=' + str(bus),
                           'XDG_CURRENT_DESKTOP': 'sway', 'XDG_SESSION_TYPE': 'wayland', 'DISPLAY': ':0'}
            value = {'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                     'socket': str(socket), 'environment': environment}
            original_lstat = Path.lstat

            def observed_lstat(path, *args, **kwargs):
                if path == bus:
                    return SimpleNamespace(st_mode=stat.S_IFSOCK | 0o600, st_uid=account.pw_uid)
                return original_lstat(path, *args, **kwargs)

            routing.write_bytes(json.dumps(value).encode('utf-8'))
            routing.chmod(0o600)
            with patch.object(guest, 'HOME', home), patch.object(Path, 'lstat', observed_lstat):
                self.assertEqual(guest.desktop_environment(account, socket), environment)
                value['boot_id'] = 'different-boot'
                routing.write_bytes(json.dumps(value).encode('utf-8'))
                with self.assertRaisesRegex(RuntimeError, 'another compositor or boot'):
                    guest.desktop_environment(account, socket)

    def test_inhibitor_must_belong_to_the_candidate_daemon(self):
        row={'who':'sway-session','uid':1000,'pid':123,'what':'shutdown:sleep','mode':'delay'}
        want = dict(row)
        self.assertEqual(guest.inhibitor_evidence(json.dumps([row]),123,1000),want)
        table='WHO UID USER PID COMM WHAT WHY MODE\nsway-session 1000 ss-test 123 sway-session shutdown:sleep Finish persistent session state work delay\n\n1 inhibitors listed.\n'
        self.assertEqual(guest.inhibitor_evidence(table,123,1000),want)
        for changed in [{'pid':456},{'uid':1001},{'mode':'block'},{'what':'shutdown'},{'who':'other'}]:
            with self.assertRaisesRegex(RuntimeError,'exact daemon'):
                guest.inhibitor_evidence(json.dumps([dict(row,**changed)]),123,1000)
        for missing in ['', 'No inhibitors.\n', json.dumps([row,row])]:
            with self.assertRaises(RuntimeError):
                guest.inhibitor_evidence(missing,123,1000)

if __name__=='__main__':
    unittest.main()
