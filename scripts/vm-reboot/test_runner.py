"""Exercise the evidence-checking CLI with synthetic, explicitly non-live fixtures."""
import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

RUNNER = Path(__file__).resolve().parents[1] / 'verify-vm-reboot.py'

class EvidenceCLI(unittest.TestCase):
    def bundle(self):
        layout = {'workspaces': [{'name': '98', 'restore_mode': 'layout', 'tiling': {'layout': 'tabbed', 'children': [{'context_id': 'a'}, {'context_id': 'b'}]}, 'floating': []}]}
        artifact = {'sha256': 'a'*64, 'build': {'product_version': 'dev', 'commit': 'b'*40, 'modified': False}}
        observations = []
        for boot in ['boot-a', 'boot-b', 'boot-c']:
            observations.append({'boot_id': boot, 'run_id': 'fixture', 'live': copy.deepcopy(layout), 'stored': copy.deepcopy(layout), 'contexts': [{'id': 'a', 'state': 'active'}, {'id': 'b', 'state': 'active'}], 'staging_managed_windows': 0, 'daemon_binary_current': True, 'delay_inhibitor': True, 'artifact': copy.deepcopy(artifact)})
        return {'source': 'qemu-kvm', 'run_id': 'fixture', 'artifact': artifact, 'observations': observations, 'reboot_requests': [
            {'accepted': True, 'method': 'guest systemd timer -> systemctl reboot', 'before_boot_id': 'boot-a', 'after_boot_id': 'boot-b'},
            {'accepted': True, 'method': 'guest systemd timer -> systemctl reboot', 'before_boot_id': 'boot-b', 'after_boot_id': 'boot-c'}]}

    def check(self, evidence):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root)/'evidence.json'
            path.write_text(json.dumps(evidence))
            return subprocess.run([sys.executable, str(RUNNER), '--check-evidence', str(path)], capture_output=True, text=True, timeout=10)

    def test_matching_two_reboots_are_accepted_by_evidence_checker(self):
        self.assertEqual(self.check(self.bundle()).returncode, 0)

    def test_unaccepted_or_unrelated_reboots_are_rejected(self):
        for change in [{'accepted': False}, {'before_boot_id': 'unexpected-boot'}]:
            evidence = self.bundle(); evidence['reboot_requests'][0].update(change)
            self.assertEqual(self.check(evidence).returncode, 1)

    def test_same_boot_is_rejected(self):
        evidence = self.bundle(); evidence['observations'][1]['boot_id'] = 'boot-a'
        self.assertEqual(self.check(evidence).returncode, 1)

    def test_lost_tabbed_layout_is_rejected(self):
        evidence = self.bundle(); evidence['observations'][1]['live']['workspaces'][0]['tiling']['layout'] = 'splith'
        self.assertEqual(self.check(evidence).returncode, 1)

    def test_second_reboot_must_match_original_child_order(self):
        evidence = self.bundle()
        for kind in ['live', 'stored']:
            evidence['observations'][2][kind]['workspaces'][0]['tiling']['children'].reverse()
        self.assertEqual(self.check(evidence).returncode, 1)

    def test_lost_context_and_staging_are_rejected(self):
        for key, value in [('contexts', []), ('staging_managed_windows', 1)]:
            evidence = self.bundle(); evidence['observations'][1][key] = value
            self.assertEqual(self.check(evidence).returncode, 1)

    def test_wrong_candidate_daemon_or_inhibitor_are_rejected(self):
        for key in ['daemon_binary_current', 'delay_inhibitor']:
            evidence = self.bundle(); evidence['observations'][2][key] = False
            self.assertEqual(self.check(evidence).returncode, 1)
        evidence = self.bundle(); evidence['observations'][1]['artifact']['sha256'] = 'c'*64
        self.assertEqual(self.check(evidence).returncode, 1)

    def test_non_tabbed_or_single_window_baseline_is_rejected(self):
        for mode in ['splith', 'tabbed']:
            evidence = self.bundle()
            for obs in evidence['observations']:
                for kind in ['live', 'stored']:
                    obs[kind]['workspaces'][0]['tiling']['layout'] = mode
                    if mode == 'tabbed':
                        obs[kind]['workspaces'][0]['tiling']['children'].pop()
            self.assertEqual(self.check(evidence).returncode, 1)

if __name__ == '__main__':
    unittest.main()
