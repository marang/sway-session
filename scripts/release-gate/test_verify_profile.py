"""The timing wrapper must preserve command failures and uncached Go flags."""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location(
    "verify_profile", Path(__file__).resolve().parents[1] / "verify-profile.py")
PROFILE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROFILE)


class VerifyProfileTest(unittest.TestCase):
    def test_failed_command_remains_failed_and_records_diagnostics(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            with contextlib.redirect_stdout(io.StringIO()):
                result = PROFILE.run_step("fixture", ["sh", "-c", "echo failure; exit 7"], output)
            self.assertEqual(result, 7)
            self.assertEqual(json.loads((output / "fixture.json").read_text())["exit_code"], 7)
            self.assertIn("failure", (output / "fixture.log").read_text())

    def test_go_events_keep_race_and_count_flags_and_report_failed_test(self):
        events = [
            {"Action": "start", "Package": "example"},
            {"Action": "output", "Package": "example", "Output": "specific failure\n"},
            {"Action": "fail", "Package": "example", "Test": "TestBroken", "Elapsed": 0.2},
            {"Action": "fail", "Package": "example", "Elapsed": 0.5},
        ]
        process = mock.Mock(stdout=io.StringIO("".join(json.dumps(event) + "\n" for event in events)))
        process.wait.return_value = 1
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            console = io.StringIO()
            with mock.patch.object(PROFILE.subprocess, "Popen", return_value=process) as popen:
                with contextlib.redirect_stdout(console):
                    result = PROFILE.run_step("race", ["go", "test", "-race", "-count=1", "./..."], output)
            self.assertEqual(result, 1)
            self.assertEqual(popen.call_args.args[0], ["go", "test", "-json", "-race", "-count=1", "./..."])
            self.assertIn("specific failure", console.getvalue())
            package = json.loads((output / "race.json").read_text())["packages"]["example"]
            self.assertEqual(package["execution_seconds"], 0.5)
            self.assertGreaterEqual(package["pre_execution_seconds"], 0)

    def test_complete_gate_is_bounded_and_failure_is_not_hidden(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            with mock.patch.object(PROFILE.subprocess, "call", return_value=2) as call:
                with mock.patch.object(PROFILE.subprocess, "check_output", return_value="go version go1.26.5 linux/amd64\n"):
                    with mock.patch.dict(os.environ, {}, clear=True):
                        with contextlib.redirect_stdout(io.StringIO()):
                            result = PROFILE.profile(output)
            self.assertEqual(result, 2)
            self.assertEqual(call.call_args.args[0], ["make", "--jobs=2", "--output-sync=target", "verify"])
            self.assertEqual(call.call_args.kwargs["env"]["VERIFY_PROFILE_DIR"], str(output))
            self.assertEqual(json.loads((output / "profile.json").read_text())["exit_code"], 2)

    def test_historical_makefile_uses_a_serial_complete_gate(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            with mock.patch.object(PROFILE.Path, "read_text", return_value="verify: test race lint\n"):
                with mock.patch.object(PROFILE.subprocess, "call", return_value=0) as call:
                    with mock.patch.object(PROFILE.subprocess, "check_output", return_value="fixture\n"):
                        with mock.patch.dict(os.environ, {}, clear=True):
                            with contextlib.redirect_stdout(io.StringIO()):
                                self.assertEqual(PROFILE.profile(output), 0)
            self.assertEqual(call.call_args.args[0], ["make", "--jobs=1", "--output-sync=target", "verify"])

    def test_stale_profile_cannot_be_mixed_with_a_new_run(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            (output / "test.json").write_text('{"exit_code": 0}')
            with self.assertRaisesRegex(ValueError, "must be empty"):
                PROFILE.profile(output)


if __name__ == "__main__":
    unittest.main()
