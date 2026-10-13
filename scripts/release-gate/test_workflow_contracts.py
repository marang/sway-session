"""Production workflow edges and executable verification aggregation.

These static checks inspect production YAML; shell fixtures execute its actual
aggregation block without claiming to emulate GitHub's scheduler.
"""

import copy
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
import yaml


WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
VERIFIER = "./.github/workflows/verify.yml"
RESOLVED_COMMIT = "${{ needs.resolve.outputs.commit }}"
RESOLVED_TAG = "${{ needs.resolve.outputs.tag }}"
COMMIT_MATCH = "needs.verify.outputs.commit == needs.resolve.outputs.commit"
ACCEPT_IF = "${{ always() && needs.lookup.result == 'success' && (needs.fresh.result == 'success' || needs.fresh.result == 'skipped') }}"


def load_workflow(name):
    workflow = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    if True in workflow and "on" not in workflow:
        workflow["on"] = workflow.pop(True)
    return workflow


def expression(value):
    return re.sub(r"\s+", "", value)


def dependencies(job):
    needs = job.get("needs", [])
    return {needs} if isinstance(needs, str) else set(needs)


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from strings(child)
    elif isinstance(value, list):
        for child in value:
            yield from strings(child)


def steps_using(job, action):
    return [step for step in job.get("steps", []) if step.get("uses", "").startswith(action + "@")]


def named_step(job, name):
    return next(step for step in job["steps"] if step["name"] == name)


class WorkflowContractsTest(unittest.TestCase):
    def setUp(self):
        self.verify = load_workflow("verify.yml")
        self.gate = load_workflow("verify-release.yml")
        self.ci = load_workflow("ci.yml")
        self.release = load_workflow("publish-release.yml")
        self.aur = load_workflow("aur-package.yml")
        self.recovery = load_workflow("publish-aur.yml")

    def assert_success_only(self, item):
        self.assertIn(item.get("continue-on-error"), (None, False))
        self.assertNotRegex(item.get("if", ""), r"\b(?:always|failure|cancelled)\s*\(")

    def assert_requires_success(self, job, required):
        self.assertTrue(set(required) <= dependencies(job))
        self.assert_success_only(job)
        for step in job.get("steps", []):
            self.assert_success_only(step)

    def assert_commit_gate(self, job, required, suffix=""):
        self.assert_requires_success(job, required)
        self.assertEqual(expression(job.get("if", "")), expression("${{ " + COMMIT_MATCH + suffix + " }}"))

    def assert_verifier_call(self, job, verifier, commit):
        self.assertEqual(job["uses"], verifier)
        self.assertEqual(expression(job["with"]["commit"]), expression(commit))
        self.assertNotIn("secrets", job)
        self.assert_success_only(job)

    def assert_aggregation(self, gate):
        lookup, fresh, accept = (gate["jobs"][name] for name in ("lookup", "fresh", "accept"))
        self.assert_requires_success(lookup, set())
        self.assert_requires_success(fresh, {"lookup"})
        self.assertEqual(expression(fresh["if"]), expression("${{ needs.lookup.outputs.commit != inputs.commit }}"))
        self.assert_verifier_call(fresh, VERIFIER, "${{ inputs.commit }}")
        self.assertEqual(dependencies(accept), {"lookup", "fresh"})
        self.assertEqual(expression(accept.get("if", "")), expression(ACCEPT_IF))
        self.assertIn(accept.get("continue-on-error"), (None, False))
        for step in accept["steps"]:
            self.assert_success_only(step)
            self.assertNotIn("if", step)

    def test_ci_has_one_unconditional_full_verifier_and_release_has_one_fallback_gate(self):
        self.assertEqual(set(self.ci["on"]), {"push", "pull_request"})
        self.assertEqual(self.ci["on"]["push"], {"branches": ["main"]})
        self.assertEqual(set(self.ci["jobs"]), {"verify"})
        self.assert_verifier_call(self.ci["jobs"]["verify"], VERIFIER, "${{ github.sha }}")
        self.assertNotIn("if", self.ci["jobs"]["verify"])
        job = self.release["jobs"]["verify"]
        self.assert_requires_success(job, {"resolve"})
        self.assert_verifier_call(job, "./.github/workflows/verify-release.yml", RESOLVED_COMMIT)
        self.assertNotIn("if", job)
        self.assert_aggregation(self.gate)
        self.assertEqual(self.gate["on"]["workflow_call"]["outputs"]["commit"]["value"], "${{ jobs.accept.outputs.commit }}")
        for workflow in (self.verify, self.gate):
            self.assertEqual(set(workflow["on"]), {"workflow_call"})
            self.assertEqual(workflow["on"]["workflow_call"]["inputs"]["commit"],
                             {"required": True, "type": "string"})

    def test_verification_has_read_permissions_and_no_publication_secrets(self):
        for workflow, permissions in ((self.verify, {"contents": "read"}), (self.ci, {"contents": "read"}),
                                      (self.gate, {"contents": "read", "actions": "read"})):
            self.assertEqual(workflow["permissions"], permissions)
            self.assertNotIn("secrets", workflow)
            self.assertFalse(any("secrets." in value for value in strings(workflow)))
            self.assertNotIn("secrets", workflow.get("on", {}).get("workflow_call", {}))
            for job in workflow["jobs"].values():
                self.assertNotIn("secrets", job)
                self.assertEqual(job.get("permissions", permissions), permissions)
        step = named_step(self.gate["jobs"]["lookup"], "Select completed compatible main-CI proof")
        self.assertEqual(step["env"], {"GH_TOKEN": "${{ github.token }}"})
        self.assertIn('select --commit "$VERIFY_COMMIT"', step["run"])
        self.assertIn('>> "$GITHUB_OUTPUT"', step["run"])

    def test_retired_workflow_ids_retain_inert_placeholders(self):
        for name in ("release.yml", "aur.yml"):
            retired = load_workflow(name)
            self.assertEqual(set(retired["on"]), {"workflow_dispatch"})
            self.assertEqual(retired["permissions"], {})
            self.assertFalse(any("secrets." in value for value in strings(retired)))
            self.assertEqual(set(retired["jobs"]), {"retired"})
            self.assertEqual(expression(retired["jobs"]["retired"]["if"]), "${{false}}")
            self.assertEqual(retired["jobs"]["retired"]["steps"], [{"run": "exit 1"}])

    def test_historical_targets_use_staged_immutable_tooling_then_exact_requested_source(self):
        for job, commit in ((self.verify["jobs"]["verify"], "${{ inputs.commit }}"),
                            (self.gate["jobs"]["lookup"], "${{ inputs.commit }}"),
                            *((self.release["jobs"][name], RESOLVED_COMMIT)
                              for name in ("artifacts", "handoff", "github-release"))):
            checkouts = steps_using(job, "actions/checkout")
            self.assertEqual([step["with"]["ref"] for step in checkouts], ["${{ github.workflow_sha }}", commit])
            for checkout in checkouts:
                self.assertIs(checkout["with"]["persist-credentials"], False)
            between = job["steps"][job["steps"].index(checkouts[0]) + 1:job["steps"].index(checkouts[1])]
            self.assertTrue(any("cp scripts/" in step.get("run", "") and "$RUNNER_TEMP" in step["run"] for step in between))

    def test_proof_and_commit_outputs_follow_full_verification_config_check_and_proof_upload(self):
        job = self.verify["jobs"]["verify"]
        self.assert_success_only(job)
        self.assertNotIn("if", job)
        self.assertEqual(job["env"]["VERIFY_COMMIT"], "${{ inputs.commit }}")
        allowed = {"Install staticcheck": "steps.staticcheck.outputs.cache-hit != 'true'",
                   "Save module downloads": "steps.modules.outputs.cache-hit != 'true'",
                   "Save staticcheck binary": "steps.staticcheck.outputs.cache-hit != 'true'",
                   "Upload verification profile": "always()"}
        for step in job["steps"]:
            self.assertIn(step.get("continue-on-error"), (None, False))
            if "if" in step:
                self.assertEqual(step["if"], allowed[step["name"]])
            if step["name"] != "Upload verification profile":
                self.assert_success_only(step)
        diagnostic = named_step(job, "Upload verification profile")
        self.assertEqual(diagnostic["with"]["path"], "${{ runner.temp }}/verify-profile")
        self.assertEqual(diagnostic["with"]["if-no-files-found"], "warn")
        verify = named_step(job, "Verify")
        self.assertIn('"$RUNNER_TEMP/verification-tooling/verify-profile.py" --output', verify["run"])
        config = steps_using(job, "goreleaser/goreleaser-action")
        self.assertEqual(len(config), 1)
        self.assertEqual(config[0]["with"]["args"], "check")
        create = named_step(job, "Create verification proof")
        self.assertIn('create --commit "$VERIFY_COMMIT"', create["run"])
        upload = named_step(job, "Upload verification proof")
        self.assertEqual(upload["with"]["name"], "verification-proof-${{ inputs.commit }}")
        self.assertEqual(upload["with"]["if-no-files-found"], "error")
        self.assertIs(upload["with"]["overwrite"], True)
        final = job["steps"][-1]
        for earlier, later in ((verify, config[0]), (config[0], create), (create, upload), (upload, final)):
            self.assertLess(job["steps"].index(earlier), job["steps"].index(later))
            self.assertNotIn("if", earlier)
        self.assertEqual(job["outputs"]["commit"], "${{ steps." + final["id"] + ".outputs.commit }}")
        self.assertIn('test "$(git rev-parse HEAD)" = "$VERIFY_COMMIT"', final["run"])
        self.assertRegex(final["run"], r"printf\s+'commit=%s\\n'\s+\"\$VERIFY_COMMIT\"")
        self.assertIn('>> "$GITHUB_OUTPUT"', final["run"])
        self.assertEqual(self.verify["on"]["workflow_call"]["outputs"]["commit"]["value"],
                         "${{ jobs.verify.outputs.commit }}")
        preflight = "\n".join(step.get("run", "") for step in job["steps"][:job["steps"].index(verify)])
        self.assertIn('test "$(git rev-parse HEAD)" = "$VERIFY_COMMIT"', preflight)
        self.assertIn("^[0-9a-f]{40}$", preflight)

    def test_build_consumers_and_publishers_require_completed_matching_verification(self):
        self.assert_commit_gate(self.release["jobs"]["artifacts"], {"resolve", "verify"}, " && (inputs.target != 'aur' || inputs.validation-only == true)")
        self.assert_commit_gate(self.release["jobs"]["handoff"], {"resolve", "verify", "artifacts"})
        self.assert_commit_gate(self.release["jobs"]["github-release"], {"resolve", "verify", "artifacts", "handoff"}, " && inputs.validation-only != true")
        self.assert_commit_gate(self.release["jobs"]["aur"], {"resolve", "verify"}, " && inputs.validation-only != true")

    def test_one_unprivileged_package_build_and_publisher_pins_transferred_artifact_id_and_manifest(self):
        builds = [(name, step) for name, job in self.release["jobs"].items() for step in steps_using(job, "goreleaser/goreleaser-action")]
        self.assertEqual(len(builds), 1)
        name, build = builds[0]
        self.assertEqual(name, "artifacts")
        self.assertEqual(build["with"], {"version": "2.18.2", "args": "release --clean --skip=publish,announce"})
        job = self.release["jobs"][name]
        self.assertNotIn("permissions", job)
        self.assertNotIn("secrets", job)
        self.assertFalse(any("secrets." in value for value in strings(job)))
        self.assertFalse(any("github.token" in value for value in strings(build)))
        restore = named_step(job, "Reuse an already complete immutable release")
        self.assertEqual(expression(restore["if"]), expression("${{ inputs.validation-only != true }}"))
        self.assertEqual(restore["env"], {"GH_TOKEN": "${{ github.token }}"})
        self.assertIn('restore --output "$RUNNER_TEMP/release-assets"', restore["run"])
        self.assertEqual(expression(build["if"]), expression("${{ steps.restored.outputs.restored != 'true' }}"))
        self.assertLess(job["steps"].index(restore), job["steps"].index(build))
        manifest = named_step(job, "Inspect standalone packages and bind every file digest")
        self.assertNotIn("if", manifest)
        self.assertEqual(manifest["env"], {"RESTORED": "${{ steps.restored.outputs.restored }}"})
        self.assertIn('if [ "$RESTORED" = true ]; then', manifest["run"])
        self.assertIn('release_artifacts.py" verify --directory', manifest["run"])
        self.assertIn('release_artifacts.py" stage --dist dist', manifest["run"])
        upload = steps_using(job, "actions/upload-artifact")
        self.assertEqual(len(upload), 1)
        self.assertIn("github.run_attempt", upload[0]["with"]["name"])
        self.assertEqual(upload[0]["with"]["if-no-files-found"], "error")
        self.assertEqual(job["outputs"]["artifact-id"], "${{ steps.upload.outputs.artifact-id }}")
        self.assertEqual(job["outputs"]["manifest-sha256"], "${{ steps.manifest.outputs.sha256 }}")
        for name, operation in (("handoff", "verify"), ("github-release", "publish")):
            job = self.release["jobs"][name]
            downloads = steps_using(job, "actions/download-artifact")
            self.assertEqual(len(downloads), 1)
            self.assertEqual(downloads[0]["with"]["artifact-ids"], "${{ needs.artifacts.outputs.artifact-id }}")
            self.assertNotIn("name", downloads[0]["with"])
            self.assertEqual(job["env"]["MANIFEST_SHA256"], "${{ needs.artifacts.outputs.manifest-sha256 }}")
            script = next(step["run"] for step in job["steps"]
                          if 'release_artifacts.py" ' + operation in step.get("run", ""))
            self.assertIn('test "$(sha256sum release-assets/release-manifest.json', script)
            self.assertIn('"$MANIFEST_SHA256"', script)
            self.assertIn('release_artifacts.py" ' + operation, script)
            self.assertFalse(steps_using(job, "goreleaser/goreleaser-action"))
        self.assertEqual(self.release["jobs"]["github-release"]["permissions"], {"contents": "write", "actions": "read"})

    def test_dispatch_validation_cannot_publish_and_manual_recovery_is_main_only(self):
        self.assertEqual(set(self.release["on"]), {"push", "workflow_call", "workflow_dispatch"})
        self.assertEqual(self.release["on"]["push"], {"tags": ["v*"]})
        self.assertIs(self.release["on"]["workflow_dispatch"]["inputs"]["validation-only"]["default"], True)
        resolve = self.release["jobs"]["resolve"]
        self.assertEqual(resolve["env"]["EXPECTED_COMMIT"], "${{ github.event_name == 'push' && github.sha || '' }}")
        self.assertEqual(resolve["env"]["VERSION"], "${{ inputs.version || github.ref_name }}")
        self.assertIn('test "$GITHUB_REF" = refs/heads/main', resolve["steps"][-1]["run"])
        self.assertIn('"$VERSION" "$EXPECTED_COMMIT"', resolve["steps"][-1]["run"])
        self.assertEqual(set(self.recovery["on"]), {"workflow_dispatch"})
        recover = self.recovery["jobs"]["recover"]
        self.assertEqual(expression(recover["if"]), expression("${{ github.ref == 'refs/heads/main' && inputs.operation == 'publish-release' }}"))
        self.assertEqual(recover["uses"], "./.github/workflows/publish-release.yml")
        self.assertEqual(recover["with"], {"version": "${{ inputs.version }}", "target": "aur", "validation-only": False})
        self.assertEqual(expression(self.recovery["jobs"]["verify-release-sync-token"]["if"]), expression("${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && inputs.operation == 'verify-sync-token' }}"))

    def test_publication_credentials_remain_confined_to_explicit_gated_publishers(self):
        self.assertEqual(self.release["permissions"], {"contents": "read", "actions": "read"})
        self.assertEqual(self.aur["permissions"], {"contents": "read"})
        self.assertEqual(self.recovery["permissions"], {"contents": "read", "actions": "read"})
        for workflow, allowed in ((self.release, {"aur"}), (self.aur, {"publish-aur", "sync-pkgbuild"}), (self.recovery, {"recover", "verify-release-sync-token"})):
            actual = {name for name, job in workflow["jobs"].items() if any("secrets." in value for value in strings(job))}
            self.assertEqual(actual, allowed)
            self.assertFalse(any("secrets." in value for value in strings(workflow.get("env", {}))))
            for job in workflow["jobs"].values():
                self.assertNotEqual(job.get("secrets"), "inherit")
                for checkout in steps_using(job, "actions/checkout"):
                    self.assertIs(checkout["with"]["persist-credentials"], False)
        for name, job in self.release["jobs"].items():
            if name != "github-release":
                self.assertEqual(job.get("permissions", self.release["permissions"]), self.release["permissions"])

    def test_aur_preserves_source_build_metadata_and_host_key_guards(self):
        call = self.release["jobs"]["aur"]
        self.assertEqual(call["uses"], "./.github/workflows/aur-package.yml")
        self.assertEqual(call["with"], {"tag": RESOLVED_TAG, "commit": RESOLVED_COMMIT, "verified-commit": "${{ needs.verify.outputs.commit }}"})
        for name in ("build", "publish-aur"):
            self.assertEqual(expression(self.aur["jobs"][name]["if"]), expression("${{ inputs.commit == inputs.verified-commit }}"))
        self.assert_requires_success(self.aur["jobs"]["publish-aur"], {"build"})
        self.assert_requires_success(self.aur["jobs"]["sync-pkgbuild"], {"publish-aur"})
        build = self.aur["jobs"]["build"]
        self.assertEqual(build["container"], "archlinux:latest")
        self.assertEqual(steps_using(build, "actions/checkout")[0]["with"]["ref"], "${{ inputs.commit }}")
        scripts = "\n".join(step.get("run", "") for step in build["steps"])
        for guard in ('"$tag_commit" != "$EXPECTED_COMMIT"', '"$head_commit" != "$tag_commit"', 'merge-base --is-ancestor "$tag_commit" origin/main', "sudo -u build makepkg --syncdeps --cleanbuild --clean --noconfirm", "makepkg --printsrcinfo"):
            self.assertIn(guard, scripts)
        upload = steps_using(build, "actions/upload-artifact")[0]
        self.assertEqual(upload["with"]["if-no-files-found"], "error")
        guards = []
        for name, guard_name, mutation_name, target, empty, equal in (("publish-aur", "Reject stale package metadata", "Commit & push to AUR", "aur", "true", "false"), ("sync-pkgbuild", "Reject stale repository metadata", "Update exact release metadata and open PR", ".", "false", "true")):
            job = self.aur["jobs"][name]
            self.assertEqual(steps_using(job, "actions/download-artifact")[0]["with"]["name"], upload["with"]["name"])
            guard, mutation = named_step(job, guard_name), named_step(job, mutation_name)
            self.assertLess(job["steps"].index(guard), job["steps"].index(mutation))
            self.assertNotIn("if", guard)
            self.assertEqual(guard["env"], {"CURRENT_METADATA_DIR": target, "ALLOW_EMPTY_AUR": empty, "ALLOW_EQUAL_CHANGES": equal})
            guards.append(guard["run"])
        self.assertEqual(guards[0], guards[1])
        publish = self.aur["jobs"]["publish-aur"]
        host, clone = named_step(publish, "Install pinned AUR host key"), named_step(publish, "Clone AUR repo")
        self.assertLess(publish["steps"].index(host), publish["steps"].index(clone))
        self.assertIn("SHA256:RFzBCUItH9LZS0cKB5UE6ceAYhBD5C8GeOBip8Z11+4", host["run"])
        self.assertNotIn("ssh-keyscan", host["run"])
        self.assertEqual(clone["env"]["GIT_SSH_COMMAND"], "ssh -o StrictHostKeyChecking=yes")

    def test_mutations_cannot_remove_edges_or_bypass_sha_and_success(self):
        publisher = self.release["jobs"]["github-release"]
        for mutation in ({"needs": ["resolve", "artifacts", "handoff"]}, {"if": "${{ always() && " + COMMIT_MATCH + " && inputs.validation-only != true }}"}, {"continue-on-error": True}, {"if": "${{ needs.verify.outputs.commit == github.sha && inputs.validation-only != true }}"}):
            with self.subTest(mutation=mutation), self.assertRaises(AssertionError):
                self.assert_commit_gate({**publisher, **mutation}, {"resolve", "verify", "artifacts", "handoff"}, " && inputs.validation-only != true")
        for job, mutation in (("accept", {"needs": ["lookup"]}), ("accept", {"if": "${{ always() }}"}), ("fresh", {"if": "${{ false }}"})):
            mutated = copy.deepcopy(self.gate)
            mutated["jobs"][job].update(mutation)
            with self.subTest(job=job, mutation=mutation), self.assertRaises(AssertionError):
                self.assert_aggregation(mutated)


class VerificationAggregationTest(unittest.TestCase):
    def setUp(self):
        self.job = load_workflow("verify-release.yml")["jobs"]["accept"]
        self.step = self.job["steps"][0]
        self.commit = "a" * 40

    def decision(self, reused="", fresh="", result="skipped", origin="ci-push-main", expected=None):
        environment = {"PATH": "/usr/bin:/bin", "LC_ALL": "C", "LANG": "C", "EXPECTED_COMMIT": self.commit if expected is None else expected, "REUSED_COMMIT": reused, "FRESH_COMMIT": fresh, "FRESH_RESULT": result, "REUSED_ORIGIN": origin}
        with tempfile.TemporaryDirectory(prefix="sway-session-verification-gate-") as temp:
            output = Path(temp) / "output"
            environment["GITHUB_OUTPUT"] = str(output)
            process = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", self.step["run"]], env=environment, cwd=temp, capture_output=True, text=True, timeout=10, check=False)
            values = dict(line.split("=", 1) for line in output.read_text().splitlines()) if output.exists() else {}
        return process.returncode, values

    def test_completed_reuse_accepts_exact_commit_with_skipped_fallback(self):
        self.assertEqual(self.decision(reused=self.commit), (0, {"commit": self.commit, "origin": "ci-push-main"}))

    def test_successful_fresh_verification_accepts_exact_commit(self):
        for reused in ("", "b" * 40):
            self.assertEqual(self.decision(reused=reused, fresh=self.commit, result="success"), (0, {"commit": self.commit, "origin": "fresh-verification"}))

    def test_missing_wrong_null_or_incomplete_evidence_cannot_accept(self):
        cases = ({}, {"reused": "b" * 40}, {"reused": self.commit, "origin": "pull-request"}, {"reused": self.commit, "result": "failure"}, {"reused": self.commit, "result": "cancelled"}, {"fresh": self.commit, "result": "skipped"}, {"fresh": "b" * 40, "result": "success"}, {"fresh": "", "result": "success"}, {"fresh": "null", "result": "success"}, {"reused": "null"}, {"fresh": self.commit, "result": "in_progress"}, {"expected": "", "reused": ""}, {"expected": "a" * 39, "reused": "a" * 39})
        for case in cases:
            with self.subTest(case=case):
                status, values = self.decision(**case)
                self.assertNotEqual(status, 0)
                self.assertEqual(values, {})

    def test_inputs_and_outputs_are_bound_to_required_jobs(self):
        self.assertEqual(self.step["env"], {"EXPECTED_COMMIT": "${{ inputs.commit }}", "REUSED_COMMIT": "${{ needs.lookup.outputs.commit }}", "FRESH_COMMIT": "${{ needs.fresh.outputs.commit }}", "FRESH_RESULT": "${{ needs.fresh.result }}", "REUSED_ORIGIN": "${{ needs.lookup.outputs.origin }}"})
        self.assertEqual(self.job["outputs"]["commit"], "${{ steps.accepted.outputs.commit }}")


class IsolatedRecoveryTest(unittest.TestCase):
    def test_failure_probe_is_validation_only_and_requires_original_producer(self):
        workflow = load_workflow("publish-release.yml")
        job = workflow["jobs"]["handoff"]
        step = named_step(job, "Exercise failed-job recovery for isolated validation")
        self.assertEqual(expression(step["if"]), expression(
            "${{ inputs.validation-only == true && inputs.exercise-recovery == true }}"))
        self.assertEqual(job["env"]["SOURCE_ATTEMPT"],
                         "${{ needs.artifacts.outputs.producer-attempt }}")
        self.assertEqual(workflow["jobs"]["artifacts"]["outputs"]["producer-attempt"],
                         "${{ steps.manifest.outputs.attempt }}")
        for attempt, producer, expected in (("1", "1", 1), ("2", "1", 0), ("2", "2", 1)):
            with self.subTest(attempt=attempt, producer=producer):
                process = subprocess.run(
                    ["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step["run"]],
                    env={"PATH": "/usr/bin:/bin", "GITHUB_RUN_ATTEMPT": attempt,
                         "SOURCE_ATTEMPT": producer}, capture_output=True, text=True,
                    timeout=10, check=False)
                self.assertEqual(process.returncode, expected, process.stdout + process.stderr)


if __name__ == "__main__":
    unittest.main()
