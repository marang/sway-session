"""Exercise actual AUR publication and metadata-sync steps against private Git.

Only workflow guard and mutation run blocks execute. Every remote uses the
file protocol, gh is a logging fixture, and PKGBUILD is inert input data.
"""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/publish-aur.yml"
JOB_STEPS = {
    "publish-aur": ("Reject stale package metadata", "Commit & push to AUR"),
    "sync-pkgbuild": ("Reject stale repository metadata", "Update exact release metadata and open PR"),
}
FAKE_GH = """import json
import os
import sys

arguments = sys.argv[1:]
with open(os.environ['FIXTURE_GH_LOG'], 'a', encoding='utf-8') as log:
    log.write(json.dumps(arguments) + '\\n')
if arguments == ['auth', 'setup-git'] or arguments[:2] == ['pr', 'list']:
    pass
elif arguments[:2] == ['pr', 'create']:
    print('fixture-pr://metadata-sync')
else:
    raise SystemExit('Unexpected fixture gh arguments: ' + repr(arguments))
"""


def metadata(version, revision="1", epoch=None, variation="original"):
    epoch_recipe = "" if epoch is None else f"epoch={epoch}\n"
    epoch_info = "" if epoch is None else f"\tepoch = {epoch}\n"
    recipe = (
        f"pkgname=sway-session\npkgver={version}\npkgrel={revision}\n"
        + epoch_recipe
        + f"# fixture recipe {variation}\n"
        + 'printf "PKGBUILD executed\\n" > "$FIXTURE_RECIPE_MARKER"\n'
    )
    info = (
        "pkgbase = sway-session\n"
        + f"\tpkgdesc = Fixture metadata {variation}\n"
        + epoch_info
        + f"\tpkgver = {version}\n\tpkgrel = {revision}\n\npkgname = sway-session\n"
    )
    return {"PKGBUILD": recipe.encode(), ".SRCINFO": info.encode()}


class PublicationFixture:
    def __init__(self, test, root, current, incoming, version, job="publish-aur", empty=False):
        self.test = test
        self.root = Path(tempfile.mkdtemp(prefix="case-", dir=root))
        self.job = job
        self.origin = self.root / "origin.git"
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.target = self.workspace if job == "sync-pkgbuild" else self.workspace / "aur"
        self.incoming = self.workspace / "release-metadata"
        self.gh_log = self.root / "gh.jsonl"
        self.recipe_marker = self.root / "recipe-executed"
        private_bin = self.root / "bin"
        private_bin.mkdir()
        hooks = self.root / "no-hooks"
        hooks.mkdir()
        gh = private_bin / "gh"
        gh.write_text("#!" + sys.executable + "\n" + FAKE_GH, encoding="utf-8")
        gh.chmod(0o700)
        # No inherited Git routing, credentials, hooks, tokens, Python paths or
        # shell startup hooks. Even accidental Git URLs permit only local files.
        self.env = {
            "PATH": str(private_bin) + os.pathsep + "/usr/bin:/bin",
            "LC_ALL": "C", "LANG": "C",
            "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
            "GIT_ALLOW_PROTOCOL": "file", "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": str(hooks),
            "GIT_AUTHOR_NAME": "AUR Version Test", "GIT_AUTHOR_EMAIL": "aur-version@example.invalid",
            "GIT_COMMITTER_NAME": "AUR Version Test", "GIT_COMMITTER_EMAIL": "aur-version@example.invalid",
            "GIT_AUTHOR_DATE": "2026-01-01T00:00:00+00:00",
            "GIT_COMMITTER_DATE": "2026-01-01T00:00:00+00:00",
            "VERSION": version, "PKGNAME": "sway-session",
            "GITHUB_WORKSPACE": str(self.workspace),
            "FIXTURE_GH_LOG": str(self.gh_log),
            "FIXTURE_RECIPE_MARKER": str(self.recipe_marker),
            "GH_TOKEN": "fixture-not-a-credential",
        }
        branch = "main" if job == "sync-pkgbuild" else "master"
        self.branch = branch
        self.git("init", "--bare", "--initial-branch=" + branch, "--object-format=sha1", "--template=", str(self.origin), cwd=self.root)
        seed = self.root / "seed"
        self.git("init", "--initial-branch=" + branch, "--object-format=sha1", "--template=", str(seed), cwd=self.root)
        if not empty:
            (seed / "identity.txt").write_text("private repository fixture\n", encoding="utf-8")
            self.write_metadata(seed, current)
            self.git("add", ".", cwd=seed)
            self.git("commit", "--quiet", "-m", "current package metadata", cwd=seed)
            self.git("remote", "add", "origin", self.origin.as_uri(), cwd=seed)
            self.git("push", "--quiet", "origin", branch, cwd=seed)
        self.git("clone", "--quiet", self.origin.as_uri(), str(self.target), cwd=self.root)
        self.incoming.mkdir()
        self.write_metadata(self.incoming, incoming, incoming=True)

    @staticmethod
    def write_metadata(directory, files, incoming=False):
        for name, data in files.items():
            path = directory / ("SRCINFO" if incoming and name == ".SRCINFO" else name)
            path.write_bytes(data)

    def git(self, *arguments, cwd=None, raw=False):
        result = subprocess.run(
            ["git", *arguments], cwd=cwd or self.target, env=self.env,
            capture_output=True, text=not raw, timeout=10, check=False,
        )
        self.test.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout if raw else result.stdout.strip()

    def remote_refs(self):
        return self.git("--git-dir=" + str(self.origin), "for-each-ref", "--format=%(refname) %(objectname)", cwd=self.root)

    def local_refs(self):
        return (
            self.git("symbolic-ref", "HEAD"),
            self.git("for-each-ref", "--format=%(refname) %(objectname)"),
        )

    def remote_file(self, name, branch=None):
        return self.git("--git-dir=" + str(self.origin), "show", (branch or self.branch) + ":" + name, cwd=self.root, raw=True)

    def gh_calls(self):
        return [json.loads(line) for line in self.gh_log.read_text(encoding="utf-8").splitlines()] if self.gh_log.exists() else []

    def target_files(self):
        files = {}
        for name in ("PKGBUILD", ".SRCINFO"):
            path = self.target / name
            if path.is_symlink():
                files[name] = ("symlink", os.readlink(path))
            elif path.is_dir():
                files[name] = ("directory",)
            elif path.exists():
                files[name] = ("file", path.read_bytes())
            else:
                files[name] = None
        return files

    def execute(self):
        workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
        steps = workflow["jobs"][self.job]["steps"]
        guard_name, mutation_name = JOB_STEPS[self.job]
        selected = [step for step in steps if step.get("name") in (guard_name, mutation_name)]
        self.test.assertTrue(any(step["name"] == mutation_name for step in selected))
        # The optional lookup permits the original mutation block to demonstrate
        # the regression before the production guard exists; no guard is emulated.
        for step in selected:
            env = self.env.copy()
            env.update({name: str(value) for name, value in step.get("env", {}).items() if "${{" not in str(value)})
            result = subprocess.run(
                ["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step["run"]],
                cwd=self.workspace, env=env, capture_output=True, text=True,
                timeout=10, check=False,
            )
            if result.returncode != 0:
                return result
        return result


class AURVersionTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sway-session-aur-version-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def fixture(self, current, incoming, version="v0.6.2", **options):
        return PublicationFixture(self, self.root, current, incoming, version, **options)

    def assert_rejected_without_effects(self, fixture):
        remote_before, local_before = fixture.remote_refs(), fixture.local_refs()
        files_before = fixture.target_files()
        result = fixture.execute()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Package version guard rejected metadata:", result.stderr)
        self.assertEqual(fixture.remote_refs(), remote_before)
        self.assertEqual(fixture.local_refs(), local_before)
        self.assertEqual(fixture.target_files(), files_before)
        self.assertEqual(fixture.gh_calls(), [])
        self.assertFalse(fixture.recipe_marker.exists())

    def assert_published(self, fixture, incoming):
        before = fixture.remote_refs()
        result = fixture.execute()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotEqual(fixture.remote_refs(), before)
        for name, data in incoming.items():
            self.assertEqual(fixture.remote_file(name), data)
        self.assertEqual(fixture.gh_calls(), [])
        self.assertFalse(fixture.recipe_marker.exists())

    def test_stale_aur_rerun_cannot_roll_back_published_metadata(self):
        current, incoming = metadata("0.6.3"), metadata("0.6.2")
        fixture = self.fixture(current, incoming)
        before = fixture.remote_refs()
        result = fixture.execute()
        self.assertEqual(fixture.remote_file(".SRCINFO"), current[".SRCINFO"],
                         "Stale actual publication rolled private remote metadata back to 0.6.2")
        self.assertEqual(fixture.remote_refs(), before)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(fixture.recipe_marker.exists())

    def test_stale_standalone_sync_rerun_cannot_create_a_rollback_pr(self):
        fixture = self.fixture(metadata("0.6.3"), metadata("0.6.2"), job="sync-pkgbuild")
        remote_before, local_before = fixture.remote_refs(), fixture.local_refs()
        result = fixture.execute()
        self.assertEqual(fixture.remote_refs(), remote_before,
                         "Stale actual sync published an automation branch proposing older metadata")
        self.assertEqual(fixture.local_refs(), local_before)
        self.assertEqual(fixture.gh_calls(), [])
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(fixture.recipe_marker.exists())

    def test_newer_effective_versions_publish_exact_verified_files(self):
        cases = (
            ("new release", metadata("0.6.1"), metadata("0.6.2"), "v0.6.2"),
            ("numeric components", metadata("0.6.9"), metadata("0.6.10"), "v0.6.10"),
            ("epoch dominates version", metadata("9.9.9", epoch="1"), metadata("0.6.2", epoch="2"), "v0.6.2"),
            ("omitted epoch is zero", metadata("9.9.9"), metadata("0.6.2", epoch="1"), "v0.6.2"),
            ("package revision", metadata("0.6.2", "1"), metadata("0.6.2", "2"), "v0.6.2"),
            ("revision subrelease", metadata("0.6.2", "1.9"), metadata("0.6.2", "1.10"), "v0.6.2"),
            ("revision dominates subrelease", metadata("0.6.2", "1.99"), metadata("0.6.2", "2"), "v0.6.2"),
            ("leading zeros compare numerically", metadata("0.06.09", "001.009", "00"), metadata("0.6.10", "1.10", "0"), "v0.6.10"),
        )
        for name, current, incoming, tag in cases:
            with self.subTest(case=name):
                self.assert_published(self.fixture(current, incoming, tag), incoming)

    def test_older_effective_versions_fail_before_publication_or_sync(self):
        cases = (
            ("epoch dominates version", metadata("0.6.1", epoch="2"), metadata("9.9.9", epoch="1"), "v9.9.9"),
            ("missing epoch is zero", metadata("0.6.1", epoch="001"), metadata("9.9.9"), "v9.9.9"),
            ("numeric components", metadata("0.6.10"), metadata("0.6.9"), "v0.6.9"),
            ("package revision", metadata("0.6.2", "2"), metadata("0.6.2", "1"), "v0.6.2"),
            ("revision subrelease", metadata("0.6.2", "1.10"), metadata("0.6.2", "1.9"), "v0.6.2"),
            ("revision dominates subrelease", metadata("0.6.2", "002"), metadata("0.6.2", "1.99"), "v0.6.2"),
            ("leading zeros compare numerically", metadata("0.06.10", "01.010", "00"), metadata("0.6.9", "1.9", "0"), "v0.6.9"),
        )
        for job in JOB_STEPS:
            for name, current, incoming, tag in cases:
                with self.subTest(job=job, case=name):
                    self.assert_rejected_without_effects(self.fixture(current, incoming, tag, job=job))

    def test_exact_metadata_rerun_keeps_remote_and_local_refs(self):
        files = metadata("0.6.2")
        for job in JOB_STEPS:
            with self.subTest(job=job):
                fixture = self.fixture(files, files, job=job)
                remote_before, local_before = fixture.remote_refs(), fixture.local_refs()
                result = fixture.execute()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(fixture.remote_refs(), remote_before)
                self.assertEqual(fixture.local_refs(), local_before)
                for name, data in files.items():
                    self.assertEqual(fixture.remote_file(name), data)
                expected_calls = [["auth", "setup-git"]] if job == "sync-pkgbuild" else []
                self.assertEqual(fixture.gh_calls(), expected_calls)
                self.assertFalse(fixture.recipe_marker.exists())

    def test_changed_metadata_at_same_aur_version_requires_a_revision(self):
        current = metadata("0.6.2")
        recipe_only = dict(current, PKGBUILD=current["PKGBUILD"] + b"# changed recipe\n")
        info_only = dict(current)
        info_only[".SRCINFO"] = current[".SRCINFO"].replace(b"Fixture metadata original", b"Fixture metadata changed")
        cases = (
            ("recipe changed", current, recipe_only),
            ("srcinfo changed", current, info_only),
            ("both changed", current, metadata("0.6.2", variation="changed")),
            ("equal numeric version spelling", metadata("00.06.02", "001.00", "00"), metadata("0.6.2", "1.0", "0")),
        )
        for name, existing, incoming in cases:
            with self.subTest(case=name):
                self.assert_rejected_without_effects(self.fixture(existing, incoming))

    def test_same_version_template_sync_opens_pr_with_exact_release_files(self):
        current = metadata("0.6.2", variation="tag-template")
        incoming = metadata("0.6.2", variation="commit-pinned")
        fixture = self.fixture(current, incoming, job="sync-pkgbuild")
        main_before = fixture.git("--git-dir=" + str(fixture.origin), "rev-parse", "main", cwd=fixture.root)
        result = fixture.execute()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(fixture.git("--git-dir=" + str(fixture.origin), "rev-parse", "main", cwd=fixture.root), main_before)
        for name, data in incoming.items():
            self.assertEqual(fixture.remote_file(name, "automation/aur-v0.6.2"), data)
            self.assertEqual(fixture.remote_file(name, "main"), current[name])
        self.assertEqual([call[:2] for call in fixture.gh_calls()], [["auth", "setup-git"], ["pr", "list"], ["pr", "create"]])
        self.assertFalse(fixture.recipe_marker.exists())

    def test_malformed_srcinfo_and_package_identity_fail_closed(self):
        valid = metadata("0.6.2")[".SRCINFO"]
        malformed = {
            "duplicate pkgbase": valid.replace(b"pkgbase = sway-session\n", b"pkgbase = sway-session\npkgbase = sway-session\n"),
            "duplicate pkgver": valid.replace(b"\tpkgver = 0.6.2\n", b"\tpkgver = 0.6.2\n\tpkgver = 0.6.2\n"),
            "duplicate pkgrel": valid.replace(b"\tpkgrel = 1\n", b"\tpkgrel = 1\n\tpkgrel = 1\n"),
            "duplicate epoch": valid.replace(b"\tpkgver", b"\tepoch = 0\n\tepoch = 0\n\tpkgver"),
            "duplicate pkgname": valid + b"pkgname = sway-session\n",
            "version after pkgname": valid.replace(b"\tpkgver = 0.6.2\n", b"") + b"\tpkgver = 0.6.2\n",
            "identity after pkgname": valid.replace(b"pkgbase = sway-session\n", b"") + b"pkgbase = sway-session\n",
            "epoch after pkgname": valid + b"\tepoch = 0\n",
            "missing pkgbase": valid.replace(b"pkgbase = sway-session\n", b""),
            "missing pkgver": valid.replace(b"\tpkgver = 0.6.2\n", b""),
            "missing pkgrel": valid.replace(b"\tpkgrel = 1\n", b""),
            "missing pkgname": valid.replace(b"pkgname = sway-session\n", b""),
            "wrong pkgbase": valid.replace(b"pkgbase = sway-session", b"pkgbase = other-package"),
            "wrong pkgname": valid.replace(b"pkgname = sway-session", b"pkgname = other-package"),
            "unsupported epoch": valid.replace(b"\tpkgver", b"\tepoch = -1\n\tpkgver"),
            "unsupported version": valid.replace(b"0.6.2", b"0.6.2-rc.1"),
            "unsupported revision": valid.replace(b"pkgrel = 1", b"pkgrel = 1.2.3"),
            "zero revision": valid.replace(b"pkgrel = 1", b"pkgrel = 0"),
            "empty field": valid.replace(b"pkgrel = 1", b"pkgrel = "),
            "malformed field": valid + b"not a field\n",
            "injection value": valid.replace(b"0.6.2", b'0.6.$(printf unsafe > "$FIXTURE_RECIPE_MARKER")'),
            "invalid utf8": valid + b"\xff\n",
        }
        for name, info in malformed.items():
            for side in ("incoming", "current"):
                with self.subTest(case=name, side=side):
                    files = metadata("0.6.2")
                    files[".SRCINFO"] = info
                    current, incoming = (metadata("0.6.1"), files) if side == "incoming" else (files, metadata("0.6.2"))
                    self.assert_rejected_without_effects(self.fixture(current, incoming))

    def test_incoming_metadata_must_match_a_strict_resolved_tag(self):
        for tag in ("v0.6.3", "0.6.2", "v00.6.2", "v0.6.2-rc.1", "v0.6.2\n", 'v0.6.2$(printf unsafe > "$FIXTURE_RECIPE_MARKER")'):
            with self.subTest(tag=tag):
                self.assert_rejected_without_effects(self.fixture(metadata("0.6.1"), metadata("0.6.2"), tag))

    def test_metadata_files_must_be_bounded_regular_files(self):
        for side in ("incoming", "current"):
            for name in ("PKGBUILD", ".SRCINFO"):
                for kind in ("symlink", "directory", "oversized"):
                    with self.subTest(side=side, file=name, kind=kind):
                        fixture = self.fixture(metadata("0.6.1"), metadata("0.6.2"))
                        directory = fixture.incoming if side == "incoming" else fixture.target
                        path = directory / ("SRCINFO" if side == "incoming" and name == ".SRCINFO" else name)
                        data = path.read_bytes()
                        path.unlink()
                        if kind == "symlink":
                            regular = fixture.root / "regular-metadata"
                            regular.write_bytes(data)
                            path.symlink_to(regular)
                        elif kind == "directory":
                            path.mkdir()
                        else:
                            path.write_bytes(b"x" * (1024 * 1024 + 1))
                        self.assert_rejected_without_effects(fixture)

    def test_partial_or_missing_metadata_in_nonempty_repositories_is_rejected(self):
        valid = metadata("0.6.2")
        for files in ({}, {"PKGBUILD": valid["PKGBUILD"]}, {".SRCINFO": valid[".SRCINFO"]}):
            for side in ("incoming", "current"):
                for job in JOB_STEPS:
                    with self.subTest(files=tuple(files), side=side, job=job):
                        current, incoming = (metadata("0.6.1"), files) if side == "incoming" else (files, valid)
                        self.assert_rejected_without_effects(self.fixture(current, incoming, job=job))

    def test_first_publication_accepts_only_a_genuinely_ref_empty_aur_repository(self):
        incoming = metadata("0.0.0")
        fixture = self.fixture({}, incoming, "v0.0.0", empty=True)
        self.assertEqual(fixture.remote_refs(), "")
        self.assert_published(fixture, incoming)


if __name__ == "__main__":
    unittest.main()
