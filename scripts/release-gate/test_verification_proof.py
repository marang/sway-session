"""Adversarial REST/archive fixtures for exact-commit CI evidence reuse."""

import copy
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import urllib.error
import urllib.parse
import urllib.request
import zipfile

import verification_proof as proof


COMMIT = "a" * 40
PREFIX = "/repos/" + proof.REPOSITORY
EXPECTED = {"commit": COMMIT, "toolchain": "go1.26.5", "contract_sha256": "b" * 64}


def archive_bytes(document, name="verification-proof.json", mode=stat.S_IFREG | 0o644,
                  extra=None):
    output = io.BytesIO()
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        info = zipfile.ZipInfo(name)
        info.create_system = 3
        info.external_attr = mode << 16
        archive.writestr(info, json.dumps(document))
        if extra:
            archive.writestr(extra, "untrusted extra")
    return output.getvalue()


class FakeREST:
    def __init__(self):
        self.calls = []
        self.repository = {"id": 12, "full_name": proof.REPOSITORY}
        self.workflow = {"id": 23, "path": proof.WORKFLOW}
        self.run = {"id": 34, "run_attempt": 2, "workflow_id": 23,
                    "status": "completed", "conclusion": "success", "event": "push",
                    "head_branch": "main", "head_sha": COMMIT, "path": proof.WORKFLOW,
                    "repository": copy.deepcopy(self.repository),
                    "head_repository": copy.deepcopy(self.repository)}
        self.document = {"schema": 1, **EXPECTED, "repository": proof.REPOSITORY,
                         "event": "push", "ref": "refs/heads/main", "run_id": 34,
                         "run_attempt": 2, "workflow_sha": COMMIT}
        self.artifact = {"id": 45, "name": "verification-proof-" + COMMIT,
                         "expired": False,
                         "workflow_run": {"id": 34, "repository_id": 12,
                                          "head_repository_id": 12, "head_sha": COMMIT,
                                          "head_branch": "main"}}
        self.items = [self.artifact]
        self.current = None
        self.refresh_archive()

    def refresh_archive(self, **kwargs):
        self.content = archive_bytes(self.document, **kwargs)
        self.artifact.update(digest="sha256:" + hashlib.sha256(self.content).hexdigest(),
                             size_in_bytes=len(self.content))

    def json(self, path):
        self.calls.append(path)
        if path == PREFIX:
            return copy.deepcopy(self.repository)
        if path == PREFIX + "/actions/workflows/ci.yml":
            return copy.deepcopy(self.workflow)
        if path.startswith(PREFIX + "/actions/workflows/ci.yml/runs?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)
            assert query == {"head_sha": [COMMIT], "event": ["push"], "branch": ["main"],
                             "status": ["success"], "per_page": [str(proof.MAX_RUNS)]}
            return {"workflow_runs": [copy.deepcopy(self.run)]}
        if path == PREFIX + "/actions/runs/34":
            reads = self.calls.count(path)
            return copy.deepcopy(self.current if reads > 1 and self.current else self.run)
        if path == PREFIX + f"/actions/runs/34/artifacts?per_page={proof.MAX_ARTIFACTS}":
            return {"total_count": len(self.items), "artifacts": copy.deepcopy(self.items)}
        raise AssertionError("Unexpected REST request: " + path)

    def archive(self, artifact_id):
        assert artifact_id == 45
        self.calls.append("download")
        return self.content


class SelectionTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeREST()

    def select(self):
        return proof.select(COMMIT, EXPECTED, self.api)

    def assert_fallback(self):
        document, outputs = self.select()
        self.assertIsNone(document)
        self.assertEqual(outputs, {"commit": "", "run_id": "", "artifact_id": "", "origin": ""})

    def test_current_successful_main_push_is_reusable(self):
        document, outputs = self.select()
        self.assertEqual(document, self.api.document)
        self.assertEqual(outputs, {"commit": COMMIT, "run_id": "34", "artifact_id": "45",
                                   "origin": "ci-push-main"})
        self.assertEqual(self.api.calls.count(PREFIX + "/actions/runs/34"), 2)

    def test_wrong_status_event_branch_commit_workflow_and_attempt_are_rejected(self):
        mutations = ({"status": "in_progress"}, {"conclusion": "skipped"},
                     {"conclusion": "failure"}, {"conclusion": "cancelled"},
                     {"conclusion": "neutral"}, {"event": "pull_request"},
                     {"event": "workflow_dispatch"}, {"head_branch": "feature"},
                     {"head_sha": "c" * 40}, {"workflow_id": 99},
                     {"path": ".github/workflows/verify.yml"}, {"run_attempt": 3},
                     {"run_attempt": True})
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.api = FakeREST()
                self.api.run.update(mutation)
                self.assert_fallback()

    def test_fork_or_wrong_repository_identity_is_rejected(self):
        for field in ("repository", "head_repository"):
            for mutation in ({"id": 99}, {"full_name": "fork/sway-session"}, {"id": None}):
                with self.subTest(field=field, mutation=mutation):
                    self.api = FakeREST()
                    self.api.run[field].update(mutation)
                    self.assert_fallback()

    def test_rerun_during_download_invalidates_proof(self):
        for mutation in ({"status": "queued"}, {"run_attempt": 3}, {"conclusion": "failure"}):
            with self.subTest(mutation=mutation):
                self.api = FakeREST()
                self.api.current = {**self.api.run, **mutation}
                self.assert_fallback()
                self.assertIn("download", self.api.calls)

    def test_missing_duplicate_expired_or_unbounded_artifact_is_rejected(self):
        self.api.items = []
        self.assert_fallback()
        self.api = FakeREST()
        self.api.items.append(copy.deepcopy(self.api.artifact))
        self.assert_fallback()
        for mutation in ({"expired": True}, {"expired": None}, {"id": True},
                         {"size_in_bytes": proof.MAX_ARCHIVE + 1}, {"digest": None},
                         {"digest": "sha256:" + "0" * 64}):
            with self.subTest(mutation=mutation):
                self.api = FakeREST()
                self.api.artifact.update(mutation)
                self.assert_fallback()
        self.api = FakeREST()
        self.api.items.extend([{}] * proof.MAX_ARTIFACTS)
        self.assert_fallback()

    def test_artifact_provenance_must_match_current_canonical_run(self):
        for mutation in ({"id": 77}, {"repository_id": 99}, {"head_repository_id": 99},
                         {"head_sha": "c" * 40}, {"head_branch": "feature"}):
            with self.subTest(mutation=mutation):
                self.api = FakeREST()
                self.api.artifact["workflow_run"].update(mutation)
                self.assert_fallback()

    def test_proof_schema_contract_toolchain_source_and_attempt_must_match(self):
        mutations = ({"schema": 2}, {"schema": True}, {"commit": "c" * 40},
                     {"contract_sha256": "d" * 64}, {"toolchain": "go1.26.4"},
                     {"repository": "fork/sway-session"}, {"event": "pull_request"},
                     {"ref": "refs/tags/v1.0.0"}, {"run_id": 35}, {"run_attempt": 1},
                     {"workflow_sha": "e" * 40})
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.api = FakeREST()
                self.api.document.update(mutation)
                self.api.refresh_archive()
                self.assert_fallback()

    def test_corrupt_archive_is_rejected_even_with_matching_api_digest(self):
        self.api.content = b"not a zip"
        self.api.artifact["digest"] = "sha256:" + hashlib.sha256(self.api.content).hexdigest()
        self.assert_fallback()

    def test_archive_traversal_symlink_extra_files_and_zip_bomb_are_rejected(self):
        for kwargs in ({"name": "../verification-proof.json"}, {"name": "/verification-proof.json"},
                       {"mode": stat.S_IFLNK | 0o777}, {"extra": "other.txt"}):
            with self.subTest(kwargs=kwargs):
                self.api = FakeREST()
                self.api.refresh_archive(**kwargs)
                self.assert_fallback()
        self.api = FakeREST()
        self.api.document["padding"] = "x" * (proof.MAX_PROOF * 10)
        self.api.refresh_archive()
        self.assert_fallback()

    def test_network_error_cannot_accept_a_proof(self):
        self.api.json = mock.Mock(side_effect=urllib.error.URLError("unavailable"))
        with self.assertRaises(urllib.error.URLError):
            self.select()

    def test_incomplete_artifact_page_does_not_hide_a_duplicate(self):
        original = self.api.json

        def incomplete(path):
            response = original(path)
            if "/artifacts?" in path:
                response["total_count"] += 1
            return response

        self.api.json = incomplete
        self.assert_fallback()


class TransportTest(unittest.TestCase):
    def test_cross_host_redirect_drops_authorization(self):
        request = urllib.request.Request("https://api.github.com/download",
                                         headers={"Authorization": "Bearer secret"})
        target = proof.SafeRedirect().redirect_request(request, None, 302, "Found", {},
                                                       "https://blob.example/file")
        self.assertFalse(target.has_header("Authorization"))

    def test_same_host_redirect_retains_authorization(self):
        request = urllib.request.Request("https://api.github.com/download",
                                         headers={"Authorization": "Bearer secret"})
        target = proof.SafeRedirect().redirect_request(request, None, 302, "Found", {},
                                                       "https://api.github.com/new")
        self.assertEqual(target.get_header("Authorization"), "Bearer secret")

    def test_plain_http_and_embedded_credentials_are_rejected(self):
        request = urllib.request.Request("https://api.github.com/download")
        for target in ("http://blob.example/file", "https://user:password@blob.example/file"):
            with self.subTest(target=target), self.assertRaises(proof.Rejected):
                proof.SafeRedirect().redirect_request(request, None, 302, "Found", {}, target)

    def test_api_read_is_bounded_and_token_cannot_go_to_payload_url(self):
        api = proof.GitHub("secret")
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b"x" * 11
        api.opener = mock.Mock()
        api.opener.open.return_value = response
        with self.assertRaises(proof.Rejected):
            api.read(PREFIX + "/actions/runs", 10)
        response.__enter__.return_value.read.assert_called_once_with(11)
        with self.assertRaises(proof.Rejected):
            api.read("https://attacker.example/file", 10)
        self.assertEqual(api.opener.open.call_count, 1)

    def test_expired_lookup_budget_prevents_network_calls(self):
        api = proof.GitHub("secret")
        api.deadline = 0
        api.opener = mock.Mock()
        with self.assertRaises(proof.Rejected):
            api.read(PREFIX + "/actions/runs", 10)
        api.opener.open.assert_not_called()


class LocalContractTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="sway-session-proof-")
        self.addCleanup(self.temp.cleanup)
        self.previous = Path.cwd()
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, self.previous)
        for name, content in (("go.mod", "module fixture\n\ngo 1.26.0\ntoolchain go1.26.5\n"),
                              ("go.sum", ""), ("Makefile", "verify:\n\ttrue\n"),
                              (".goreleaser.yaml", "version: 2\n"),
                              (".github/workflows/verify.yml", "name: Verify\n"),
                              ("scripts/check.sh", "true\n")):
            path = Path(name)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        self.git("init", "-q")
        self.git("add", ".")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "-qm", "fixture")
        self.commit = self.git("rev-parse", "HEAD").strip()
        original = proof.command
        self.commands = mock.patch.object(proof, "command", side_effect=lambda *args:
                                         "go1.26.5" if args == ("go", "env", "GOVERSION")
                                         else original(*args))
        self.commands.start()
        self.addCleanup(self.commands.stop)

    def git(self, *args):
        return subprocess.check_output(["git", *args], text=True, stderr=subprocess.DEVNULL)

    def test_clean_checkout_and_pinned_actual_toolchain_create_contract(self):
        result = proof.local_contract(self.commit)
        self.assertEqual(result["commit"], self.commit)
        self.assertEqual(result["toolchain"], "go1.26.5")
        self.assertRegex(result["contract_sha256"], r"^[0-9a-f]{64}$")
        # Generated untracked binaries do not change verified source.
        Path("sway-session").write_bytes(b"generated binary")
        self.assertEqual(proof.local_contract(self.commit), result)

    def test_wrong_checkout_or_dirty_tracked_sources_are_rejected(self):
        with self.assertRaises(proof.Rejected):
            proof.local_contract(COMMIT)
        Path("Makefile").write_text("verify:\n\tfalse\n")
        with self.assertRaises(subprocess.CalledProcessError):
            proof.local_contract(self.commit)

    def test_actual_toolchain_must_match_preferred_version(self):
        original = proof.command
        with mock.patch.object(proof, "command", side_effect=lambda *args:
                               "go1.26.4" if args == ("go", "env", "GOVERSION")
                               else original(*args)), self.assertRaises(proof.Rejected):
            proof.local_contract(self.commit)

    def test_create_records_source_sha_even_for_manual_old_tag_verification(self):
        env = {"GITHUB_REPOSITORY": proof.REPOSITORY, "GITHUB_EVENT_NAME": "workflow_dispatch",
               "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ID": "34",
               "GITHUB_RUN_ATTEMPT": "2", "GITHUB_WORKFLOW_SHA": COMMIT}
        document = proof.create(self.commit, env)
        self.assertEqual(document["workflow_sha"], COMMIT)
        self.assertEqual(document["commit"], self.commit)
        self.assertEqual(document["event"], "workflow_dispatch")

    def test_historical_source_without_shared_verifier_can_create_fresh_proof(self):
        self.git("rm", ".github/workflows/verify.yml")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "commit", "-qm", "historical source without shared verifier")
        commit = self.git("rev-parse", "HEAD").strip()
        env = {"GITHUB_REPOSITORY": proof.REPOSITORY, "GITHUB_EVENT_NAME": "workflow_dispatch",
               "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ID": "34",
               "GITHUB_RUN_ATTEMPT": "2", "GITHUB_WORKFLOW_SHA": COMMIT}
        document = proof.create(commit, env)
        self.assertEqual(document["commit"], commit)
        self.assertEqual(document["workflow_sha"], COMMIT)
        # Even a successful main-push envelope cannot turn a differently
        # sourced workflow into reusable exact-commit verification evidence.
        document.update(event="push", ref="refs/heads/main")
        expected = proof.local_contract(commit)
        run = {"id": 34, "run_attempt": 2}
        with self.assertRaisesRegex(proof.Rejected, "wrong workflow source"):
            proof.validate_proof(document, expected, run)

    def test_selector_main_network_failure_removes_stale_proof_and_emits_fallback(self):
        output = Path("selected.json")
        output.write_text("stale")
        args = ["verification_proof.py", "select", "--commit", self.commit,
                "--output", str(output)]
        with mock.patch.dict(os.environ, {"GITHUB_REPOSITORY": proof.REPOSITORY,
                                         "GH_TOKEN": "secret"}), \
                mock.patch.object(proof.sys, "argv", args), \
                mock.patch.object(proof.GitHub, "json", side_effect=urllib.error.URLError("secret")), \
                mock.patch("sys.stdout", new_callable=io.StringIO) as stdout, \
                mock.patch("sys.stderr", new_callable=io.StringIO) as stderr:
            self.assertEqual(proof.main(), 0)
            self.assertEqual(stdout.getvalue(), "commit=\nrun_id=\nartifact_id=\norigin=\n")
            self.assertNotIn("secret", stderr.getvalue())
        self.assertFalse(output.exists())

    def test_selector_main_accepts_only_checked_proof_and_writes_reviewable_json(self):
        output = Path("selected.json")
        api = FakeREST()
        args = ["verification_proof.py", "select", "--commit", COMMIT,
                "--output", str(output)]
        with mock.patch.dict(os.environ, {"GITHUB_REPOSITORY": proof.REPOSITORY,
                                         "GH_TOKEN": "secret"}), \
                mock.patch.object(proof.sys, "argv", args), \
                mock.patch.object(proof, "local_contract", return_value=EXPECTED), \
                mock.patch.object(proof, "GitHub", return_value=api), \
                mock.patch("sys.stdout", new_callable=io.StringIO) as stdout:
            self.assertEqual(proof.main(), 0)
            self.assertEqual(stdout.getvalue(), f"commit={COMMIT}\nrun_id=34\nartifact_id=45\norigin=ci-push-main\n")
        self.assertEqual(json.loads(output.read_text()), api.document)

    def test_fork_selector_does_not_make_api_calls(self):
        output = Path("selected.json")
        args = ["verification_proof.py", "select", "--commit", self.commit,
                "--output", str(output)]
        with mock.patch.dict(os.environ, {"GITHUB_REPOSITORY": "fork/sway-session"}), \
                mock.patch.object(proof.sys, "argv", args), \
                mock.patch.object(proof, "GitHub") as api, \
                mock.patch("sys.stdout", new_callable=io.StringIO) as stdout, \
                mock.patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(proof.main(), 0)
            self.assertEqual(stdout.getvalue(), "commit=\nrun_id=\nartifact_id=\norigin=\n")
        api.assert_not_called()
        self.assertFalse(output.exists())


class ActualHistoricalCheckoutTest(unittest.TestCase):
    def test_oldest_release_accepts_staged_current_proof_tooling_with_actual_pinned_go(self):
        repository = Path(__file__).resolve().parents[2]
        selected = subprocess.check_output(["go", "env", "GOVERSION", "GOROOT"],
                                           cwd=repository, text=True, timeout=30).splitlines()
        if selected[0] != "go1.26.5":
            self.skipTest("historical integration requires already-selected Go 1.26.5; "
                          "the historical source fixture still runs")
        environment = {key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ}
        environment["PATH"] = str(Path(selected[1]) / "bin") + os.pathsep + environment.get("PATH", "")
        environment.update({"LC_ALL": "C", "LANG": "C", "GOTOOLCHAIN": "local",
                            "GOPROXY": "off", "GIT_ALLOW_PROTOCOL": "file",
                            "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_GLOBAL": os.devnull,
                            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
        tag = subprocess.run(["git", "rev-parse", "--verify", "refs/tags/v0.1.0^{commit}"],
                             cwd=repository, env=environment, capture_output=True, text=True,
                             timeout=10, check=False)
        if tag.returncode:
            self.skipTest("historical v0.1.0 objects unavailable in shallow checkout")
        commit = tag.stdout.strip()
        workflow_sha = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repository,
                                               env=environment, text=True, timeout=10).strip()
        self.assertNotEqual(workflow_sha, commit)
        with tempfile.TemporaryDirectory(prefix="sway-session-proof-v010-") as temporary:
            checkout = Path(temporary) / "checkout"
            subprocess.run(["git", "clone", "--shared", "--no-hardlinks", "--no-checkout",
                            "--quiet", "--template=", str(repository), str(checkout)],
                           env=environment, check=True, capture_output=True, timeout=20)
            subprocess.run(["git", "checkout", "--quiet", "--detach", commit], cwd=checkout,
                           env=environment, check=True, capture_output=True, timeout=10)
            self.assertFalse((checkout / ".github/workflows/verify.yml").exists())
            actual = subprocess.check_output(["go", "env", "GOVERSION"], cwd=checkout,
                                              env=environment, text=True, timeout=30).strip()
            self.assertEqual(actual, "go1.26.5")
            environment.update({"GITHUB_REPOSITORY": proof.REPOSITORY,
                                "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
                                "GITHUB_RUN_ID": "34", "GITHUB_RUN_ATTEMPT": "2",
                                "GITHUB_WORKFLOW_SHA": workflow_sha})
            output = Path(temporary) / "verification-proof.json"
            created = subprocess.run([sys.executable, "-B", str(Path(proof.__file__).resolve()),
                                      "create", "--commit", commit, "--output", str(output)],
                                     cwd=checkout, env=environment, capture_output=True, text=True,
                                     timeout=30, check=False)
            self.assertEqual(created.returncode, 0, created.stderr)
            document = json.loads(output.read_text())
            self.assertEqual(document["commit"], commit)
            self.assertEqual(document["toolchain"], "go1.26.5")
            self.assertEqual(document["workflow_sha"], workflow_sha)
            self.assertEqual(document["event"], "workflow_dispatch")
            expected = {key: document[key] for key in ("commit", "toolchain", "contract_sha256")}
            document.update(event="push", ref="refs/heads/main")
            with self.assertRaisesRegex(proof.Rejected, "wrong workflow source"):
                proof.validate_proof(document, expected, {"id": 34, "run_attempt": 2})


if __name__ == "__main__":
    unittest.main()
