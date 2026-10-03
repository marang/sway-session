"""Validate narrow guest CLI/Doctor/logind reports with synthetic observations."""
import copy
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('guest_fixture', Path(__file__).with_name('guest.py'))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)

class GuestReports(unittest.TestCase):
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
