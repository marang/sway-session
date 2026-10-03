"""Check guest reboot attribution at the external SSH/boot observation boundary."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('vm_runner', Path(__file__).resolve().parents[1] / 'verify-vm-reboot.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

class GuestPeer:
    def __init__(self, before='old-boot', accepted=True):
        self.before, self.accepted = before, accepted
    def wait(self, old_boot=None, timeout=0):
        return self.before if old_boot is None else 'new-boot'
    def ssh(self, request, timeout=0):
        if not self.accepted:
            raise RuntimeError('ssh exited 255: connection refused')
        return b''

class RebootBoundary(unittest.TestCase):
    def test_acknowledged_regular_reboot_returns_the_observed_transition(self):
        request = runner.request_reboot(GuestPeer(), 'old-boot', 1)
        self.assertEqual(request, {'accepted': True, 'method': 'guest systemd timer -> systemctl reboot',
                                  'before_boot_id': 'old-boot', 'after_boot_id': 'new-boot'})
    def test_unrequested_boot_change_is_rejected_before_request(self):
        with self.assertRaisesRegex(RuntimeError, 'before the requested reboot'):
            runner.request_reboot(GuestPeer(before='unexpected-boot'), 'old-boot', 1)
    def test_failed_ssh_request_cannot_be_accepted_as_a_reboot(self):
        with self.assertRaisesRegex(RuntimeError, 'ssh exited 255'):
            runner.request_reboot(GuestPeer(accepted=False), 'old-boot', 1)

if __name__ == '__main__':
    unittest.main()
