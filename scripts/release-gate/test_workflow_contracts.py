"""Cross-workflow release gates, plus the real release rerun decision script.

These assertions inspect production workflows; they do not emulate GitHub's
scheduler. Runtime needs/result behavior is validated separately on GitHub.
"""

import copy
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import yaml


WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
VERIFIER = "./.github/workflows/verify.yml"
RESOLVED_COMMIT = "${{ needs.resolve.outputs.commit }}"
RESOLVED_TAG = "${{ needs.resolve.outputs.tag }}"
COMMIT_GATE = "${{ needs.verify.outputs.commit == needs.resolve.outputs.commit }}"


def load_workflow(name):
    workflow = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    # GitHub uses YAML 1.2, while PyYAML's safe loader treats YAML 1.1 `on`
    # as a boolean. Normalize that one top-level key without a custom parser.
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
    return [
        step for step in job.get("steps", [])
        if step.get("uses", "").startswith(action + "@")
    ]


class WorkflowContractsTest(unittest.TestCase):
    def setUp(self):
        self.verify = load_workflow("verify.yml")
        self.ci = load_workflow("ci.yml")
        self.release = load_workflow("publish-release.yml")
        self.aur = load_workflow("publish-aur.yml")

    def assert_success_only(self, item):
        self.assertIn(item.get("continue-on-error"), (None, False))
        self.assertNotRegex(
            item.get("if", ""), r"\b(?:always|failure|cancelled)\s*\(",
            "A status-function override can bypass GitHub's default success gate",
        )

    def assert_requires_success(self, job, required):
        self.assertTrue(
            set(required) <= dependencies(job),
            f"Missing successful dependencies: {set(required) - dependencies(job)}",
        )
        self.assert_success_only(job)
        for step in job.get("steps", []):
            self.assert_success_only(step)

    def assert_exact_commit_gate(self, job, required):
        self.assert_requires_success(job, required)
        self.assertEqual(expression(job.get("if", "")), expression(COMMIT_GATE))

    def assert_no_credentials(self, workflow):
        self.assertNotIn("secrets", workflow)
        self.assertFalse(any("secrets." in value for value in strings(workflow)))
        self.assertNotIn("secrets", workflow["on"].get("workflow_call", {}))
        self.assertEqual(workflow["permissions"], {"contents": "read"})
        for job in workflow["jobs"].values():
            self.assertNotIn("secrets", job)
            self.assertEqual(
                job.get("permissions", workflow["permissions"]), {"contents": "read"},
            )

    def assert_verifier_call(self, job, commit):
        self.assertEqual(job["uses"], VERIFIER)
        self.assertEqual(expression(job["with"]["commit"]), expression(commit))
        self.assertNotIn("secrets", job)
        self.assert_success_only(job)
        self.assertNotIn("if", job, "The shared verification must not be optional")

    def test_ci_and_publishers_call_one_exact_commit_verifier(self):
        self.assertEqual(set(self.ci["on"]), {"push", "pull_request"})
        self.assertEqual(self.ci["on"]["push"], {"branches": ["main"]})
        self.assert_verifier_call(self.ci["jobs"]["verify"], "${{ github.sha }}")
        for workflow in (self.release, self.aur):
            with self.subTest(workflow=workflow["name"]):
                verifier = workflow["jobs"]["verify"]
                self.assert_requires_success(verifier, {"resolve"})
                self.assert_verifier_call(verifier, RESOLVED_COMMIT)

    def test_historical_publisher_ids_retain_inert_placeholders(self):
        # Removing these paths makes GitHub's workflow state `deleted`, which
        # does not prevent historical reruns. Keep them available for disabling.
        for name in ("release.yml", "aur.yml"):
            with self.subTest(workflow=name):
                retired = load_workflow(name)
                self.assertEqual(set(retired["on"]), {"workflow_dispatch"})
                self.assertEqual(retired["permissions"], {})
                self.assertNotIn("env", retired)
                self.assertFalse(any("secrets." in value for value in strings(retired)))
                self.assertEqual(set(retired["jobs"]), {"retired"})
                job = retired["jobs"]["retired"]
                self.assertEqual(expression(job["if"]), "${{false}}")
                self.assertNotIn("uses", job)
                self.assertNotIn("permissions", job)
                self.assertEqual(job["steps"], [{"run": "exit 1"}])

    def test_verification_and_ci_have_no_publication_credentials(self):
        self.assert_no_credentials(self.verify)
        self.assert_no_credentials(self.ci)
        self.assertEqual(set(self.verify["on"]), {"workflow_call"})
        self.assertEqual(
            self.verify["on"]["workflow_call"]["inputs"]["commit"],
            {"required": True, "type": "string"},
        )

    def test_verified_output_is_written_only_after_both_required_checks(self):
        job = self.verify["jobs"]["verify"]
        self.assert_success_only(job)
        self.assertNotIn("if", job)
        self.assertEqual(job["env"]["VERIFY_COMMIT"], "${{ inputs.commit }}")
        steps = job["steps"]
        for step in steps:
            self.assert_success_only(step)
            self.assertNotIn("if", step, "Verification steps cannot be skipped")
        make_verify = [i for i, step in enumerate(steps) if step.get("run", "").strip() == "make verify"]
        self.assertEqual(len(make_verify), 1, "make verify must fail the job on failure")
        config_check = steps_using(job, "goreleaser/goreleaser-action")
        self.assertEqual(len(config_check), 1)
        self.assertEqual(config_check[0]["with"]["args"], "check")
        final_step = steps[-1]
        self.assertEqual(
            expression(job["outputs"]["commit"]),
            expression("${{ steps." + final_step["id"] + ".outputs.commit }}"),
        )
        self.assertLess(make_verify[0], steps.index(config_check[0]))
        self.assertLess(steps.index(config_check[0]), len(steps) - 1)
        output_script = final_step["run"]
        self.assertIn('test "$(git rev-parse HEAD)" = "$VERIFY_COMMIT"', output_script)
        self.assertRegex(output_script, r"printf\s+'commit=%s\\n'\s+\"\$VERIFY_COMMIT\"")
        self.assertIn('>> "$GITHUB_OUTPUT"', output_script)
        self.assertEqual(
            self.verify["on"]["workflow_call"]["outputs"]["commit"]["value"],
            "${{ jobs.verify.outputs.commit }}",
        )

    def test_verifier_checks_out_full_requested_commit_without_credentials(self):
        job = self.verify["jobs"]["verify"]
        checkout = steps_using(job, "actions/checkout")
        self.assertEqual(len(checkout), 1)
        self.assertEqual(checkout[0]["with"]["ref"], "${{ inputs.commit }}")
        self.assertIs(checkout[0]["with"]["persist-credentials"], False)
        before_verify = []
        for step in job["steps"]:
            if step.get("run", "").strip() == "make verify":
                break
            before_verify.append(step.get("run", ""))
        preflight = "\n".join(before_verify)
        self.assertIn('test "$(git rev-parse HEAD)" = "$VERIFY_COMMIT"', preflight)
        self.assertIn("^[0-9a-f]{40}$", preflight)

    def test_publish_and_package_build_require_matching_verified_commit(self):
        self.assert_exact_commit_gate(self.release["jobs"]["goreleaser"], {"resolve", "verify"})
        self.assert_exact_commit_gate(self.aur["jobs"]["build"], {"resolve", "verify"})
        self.assert_exact_commit_gate(
            self.aur["jobs"]["publish-aur"], {"resolve", "verify", "build"},
        )
        self.assert_requires_success(self.aur["jobs"]["sync-pkgbuild"], {"resolve", "publish-aur"})

    def test_publisher_checkouts_and_tags_remain_bound_to_resolved_commit(self):
        release_job = self.release["jobs"]["goreleaser"]
        build_job = self.aur["jobs"]["build"]
        for job in (release_job, build_job):
            with self.subTest(job=job.get("name", "goreleaser")):
                checkout = steps_using(job, "actions/checkout")
                self.assertEqual(len(checkout), 1)
                self.assertEqual(checkout[0]["with"]["ref"], RESOLVED_COMMIT)
        self.assertEqual(release_job["env"]["RELEASE_COMMIT"], RESOLVED_COMMIT)
        self.assertEqual(release_job["env"]["RELEASE_TAG"], RESOLVED_TAG)
        self.assertEqual(build_job["env"]["EXPECTED_COMMIT"], RESOLVED_COMMIT)
        self.assertEqual(build_job["env"]["VERSION"], RESOLVED_TAG)
        for job_name in ("publish-aur", "sync-pkgbuild"):
            self.assertEqual(self.aur["jobs"][job_name]["env"]["VERSION"], RESOLVED_TAG)
        scripts = [step.get("run", "") for step in release_job["steps"]]
        revalidation = next(i for i, script in enumerate(scripts) if "resolve-release-tag.sh" in script)
        self.assertIn('"$RELEASE_TAG" "$RELEASE_COMMIT"', scripts[revalidation])
        release_step = steps_using(release_job, "goreleaser/goreleaser-action")[0]
        self.assertEqual(release_step.get("env", {}).get("GORELEASER_CURRENT_TAG"), RESOLVED_TAG)
        self.assertLess(revalidation, release_job["steps"].index(release_step))
        build_scripts = "\n".join(step.get("run", "") for step in build_job["steps"])
        self.assertIn('"$tag_commit" != "$EXPECTED_COMMIT"', build_scripts)
        self.assertIn('"$head_commit" != "$tag_commit"', build_scripts)
        self.assertIn('merge-base --is-ancestor "$tag_commit" origin/main', build_scripts)

    def test_release_tag_resolution_preserves_push_and_manual_inputs(self):
        for workflow in (self.release, self.aur):
            with self.subTest(workflow=workflow["name"]):
                resolve = workflow["jobs"]["resolve"]
                self.assert_success_only(resolve)
                self.assertEqual(resolve["outputs"]["tag"], "${{ steps.tag.outputs.tag }}")
                self.assertEqual(resolve["outputs"]["commit"], "${{ steps.tag.outputs.commit }}")
                tag = next(step for step in resolve["steps"] if step.get("id") == "tag")
                self.assertNotIn("if", tag)
                self.assert_success_only(tag)
                self.assertIn("resolve-release-tag.sh", tag["run"])
                self.assertIn('>> "$GITHUB_OUTPUT"', tag["run"])
        release_tag = next(step for step in self.release["jobs"]["resolve"]["steps"] if step.get("id") == "tag")
        self.assertEqual(release_tag["env"]["EXPECTED_COMMIT"], "${{ github.sha }}")
        self.assertEqual(release_tag["env"]["VERSION"], "${{ github.ref_name }}")
        aur_tag = next(step for step in self.aur["jobs"]["resolve"]["steps"] if step.get("id") == "tag")
        self.assertEqual(
            aur_tag["env"]["VERSION"], "${{ github.event.inputs.version || github.ref_name }}",
        )
        self.assertEqual(
            aur_tag["env"]["EXPECTED_COMMIT"], "${{ github.event_name == 'push' && github.sha || '' }}",
        )

    def test_publication_credentials_are_in_gated_jobs_on_trusted_events(self):
        self.assertEqual(set(self.release["on"]), {"push"})
        self.assertEqual(set(self.aur["on"]), {"push", "workflow_dispatch"})
        for workflow in (self.release, self.aur):
            self.assertEqual(workflow["on"]["push"], {"tags": ["v*"]})
            self.assertEqual(workflow["permissions"], {"contents": "read"})
            for job in workflow["jobs"].values():
                for step in steps_using(job, "actions/checkout"):
                    self.assertIs(step["with"]["persist-credentials"], False)
        credential_jobs = {
            "release": {"goreleaser"},
            "aur": {"publish-aur", "sync-pkgbuild", "verify-release-sync-token"},
        }
        for name, workflow in (("release", self.release), ("aur", self.aur)):
            actual = {
                job_name for job_name, job in workflow["jobs"].items()
                if any("secrets." in value for value in strings(job))
            }
            self.assertEqual(actual, credential_jobs[name])
            self.assertFalse(any("secrets." in value for value in strings(workflow.get("env", {}))))
        self.assertEqual(self.release["jobs"]["goreleaser"]["permissions"], {"contents": "write"})
        self.assertEqual(
            expression(self.aur["jobs"]["resolve"]["if"]),
            expression("${{ github.event_name == 'push' || (github.ref == 'refs/heads/main' && inputs.operation == 'publish-release') }}"),
        )
        self.assertEqual(
            expression(self.aur["jobs"]["verify-release-sync-token"]["if"]),
            expression("${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && inputs.operation == 'verify-sync-token' }}"),
        )

    def test_aur_publication_still_consumes_successfully_built_metadata(self):
        build = self.aur["jobs"]["build"]
        self.assertEqual(build["container"], "archlinux:latest")
        build_steps = build["steps"]
        package_build = next(
            step for step in build_steps
            if "makepkg --syncdeps --cleanbuild --clean --noconfirm" in step.get("run", "")
        )
        self.assertIn("sudo -u build makepkg", package_build["run"])
        self.assertIn("makepkg --printsrcinfo", package_build["run"])
        upload = steps_using(build, "actions/upload-artifact")
        self.assertEqual(len(upload), 1)
        self.assertLess(build_steps.index(package_build), build_steps.index(upload[0]))
        self.assertEqual(upload[0]["with"]["if-no-files-found"], "error")
        self.assertEqual(upload[0]["with"]["path"].rstrip("/"), "release-metadata")
        for job_name in ("publish-aur", "sync-pkgbuild"):
            downloads = steps_using(self.aur["jobs"][job_name], "actions/download-artifact")
            self.assertEqual(len(downloads), 1)
            self.assertEqual(downloads[0]["with"]["name"], upload[0]["with"]["name"])
            self.assertEqual(downloads[0]["with"]["path"], "release-metadata")

    def test_contract_guards_reject_missing_edges_wrong_sha_and_fail_open_mutations(self):
        publisher = self.release["jobs"]["goreleaser"]
        mutations = (
            ("missing verification dependency", {"needs": ["resolve"]}),
            ("always bypass", {"if": "${{ always() && needs.verify.outputs.commit == needs.resolve.outputs.commit }}"}),
            ("ignored job failure", {"continue-on-error": True}),
            ("comparison against trigger SHA", {"if": "${{ needs.verify.outputs.commit == github.sha }}"}),
        )
        for name, change in mutations:
            with self.subTest(mutation=name):
                mutated = copy.deepcopy(publisher)
                mutated.update(change)
                with self.assertRaises(AssertionError):
                    self.assert_exact_commit_gate(mutated, {"resolve", "verify"})
        mutated = copy.deepcopy(self.release["jobs"]["verify"])
        mutated["with"]["commit"] = "${{ github.sha }}"
        with self.assertRaises(AssertionError):
            self.assert_verifier_call(mutated, RESOLVED_COMMIT)
        for name, change in (("secret inheritance", {"secrets": "inherit"}), ("write token", {"permissions": {"contents": "write"}})):
            with self.subTest(mutation=name):
                mutated = copy.deepcopy(self.ci)
                mutated["jobs"]["verify"].update(change)
                with self.assertRaises(AssertionError):
                    self.assert_no_credentials(mutated)


NODE_HARNESS = r"""
import fs from 'node:fs';
const { script, scenario } = JSON.parse(fs.readFileSync(0, 'utf8'));
const calls = [];
const outputs = {};
const github = { rest: { repos: { getReleaseByTag: async args => {
  calls.push(args);
  if (scenario === 'published') return { data: { draft: false } };
  if (scenario === 'draft') return { data: { draft: true } };
  const error = new Error('Fixture REST error');
  error.status = Number(scenario);
  throw error;
} } } };
const core = { info: () => {}, setOutput: (key, value) => { outputs[key] = value; } };
const context = { repo: { owner: 'fixture-owner', repo: 'fixture-repo' } };
const inputProcess = { env: { RELEASE_TAG: 'v1.2.3' } };
const AsyncFunction = Object.getPrototypeOf(async function() {}).constructor;
let failure = null;
try {
  await new AsyncFunction('github', 'context', 'core', 'process', script)(github, context, core, inputProcess);
} catch (error) {
  failure = { message: error.message, status: error.status ?? null };
}
console.log(JSON.stringify({ calls, outputs, failure }));
"""


class PublishedReleaseRerunTest(unittest.TestCase):
    def setUp(self):
        job = load_workflow("publish-release.yml")["jobs"]["goreleaser"]
        script_steps = steps_using(job, "actions/github-script")
        self.assertEqual(len(script_steps), 1)
        self.script_step = script_steps[0]
        self.script = self.script_step["with"]["script"]
        self.publisher = steps_using(job, "goreleaser/goreleaser-action")[0]
        self.assertLess(job["steps"].index(self.script_step), job["steps"].index(self.publisher))
        self.node = shutil.which("node")
        self.assertIsNotNone(self.node, "Node.js is required for the release rerun regression checks")

    def decision(self, scenario):
        # Avoid inheriting tokens or Node hooks. HOME is needed by local mise
        # shims; the script receives only the explicit fake process.env above.
        env = {key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ}
        env.update({"LC_ALL": "C", "LANG": "C"})
        with tempfile.TemporaryDirectory(prefix="sway-session-release-rerun-") as cwd:
            result = subprocess.run(
                [self.node, "--input-type=module", "--eval", NODE_HARNESS],
                input=json.dumps({"script": self.script, "scenario": scenario}),
                cwd=cwd, env=env, capture_output=True, text=True, timeout=15,
                check=False,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        decision = json.loads(result.stdout)
        self.assertEqual(decision["calls"], [{"owner": "fixture-owner", "repo": "fixture-repo", "tag": "v1.2.3"}])
        return decision

    def test_publisher_requires_explicit_permission_from_rerun_check(self):
        self.assertEqual(
            expression(self.publisher["if"]),
            expression("${{ steps." + self.script_step["id"] + ".outputs.publish == 'true' }}"),
        )
        self.assertNotIn("continue-on-error", self.script_step)
        self.assertNotIn("if", self.script_step)

    def test_missing_release_permits_first_publication(self):
        result = self.decision("404")
        self.assertEqual(result["outputs"], {"publish": "true"})
        self.assertIsNone(result["failure"])

    def test_published_release_preserves_existing_artifacts_on_each_rerun(self):
        for attempt in range(2):
            with self.subTest(attempt=attempt):
                result = self.decision("published")
                self.assertEqual(result["outputs"], {"publish": "false"})
                self.assertIsNone(result["failure"])

    def test_draft_requires_explicit_recovery(self):
        result = self.decision("draft")
        self.assertEqual(result["outputs"], {})
        self.assertIsNotNone(result["failure"])
        self.assertIn("draft", result["failure"]["message"])

    def test_api_errors_cannot_be_mistaken_for_missing_release(self):
        for status in (401, 403, 429, 500):
            with self.subTest(status=status):
                result = self.decision(str(status))
                self.assertEqual(result["outputs"], {})
                self.assertEqual(result["failure"]["status"], status)


if __name__ == "__main__":
    unittest.main()
