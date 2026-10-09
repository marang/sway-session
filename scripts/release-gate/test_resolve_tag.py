"""Release-tag resolution against disposable repositories and a local origin."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


RESOLVER = Path(__file__).resolve().parents[1] / "resolve-release-tag.sh"


class ResolveReleaseTagTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sway-session-release-tag-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.origin = self.root / "origin.git"
        self.repo = self.root / "checkout"
        # Exclude inherited Git routing/configuration, credentials, hooks, and
        # identity. These fixtures permit only local file-protocol remotes.
        self.env = {
            "PATH": os.environ["PATH"],
            "LC_ALL": "C",
            "LANG": "C",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_ALLOW_PROTOCOL": "file",
            "GIT_OPTIONAL_LOCKS": "0",
            "GIT_AUTHOR_NAME": "Release Gate Test",
            "GIT_AUTHOR_EMAIL": "release-gate@example.invalid",
            "GIT_COMMITTER_NAME": "Release Gate Test",
            "GIT_COMMITTER_EMAIL": "release-gate@example.invalid",
            "GIT_AUTHOR_DATE": "2026-01-01T00:00:00+00:00",
            "GIT_COMMITTER_DATE": "2026-01-01T00:00:00+00:00",
        }
        self.git(
            "init", "--bare", "--initial-branch=main", "--object-format=sha1",
            "--template=", str(self.origin), cwd=self.root,
        )
        self.git(
            "init", "--initial-branch=main", "--object-format=sha1",
            "--template=", str(self.repo), cwd=self.root,
        )
        self.initial_commit = self.commit("initial")
        self.git("remote", "add", "origin", str(self.origin))
        self.git("push", "--quiet", "--set-upstream", "origin", "main")

    def git(self, *args, cwd=None):
        result = subprocess.run(
            ["git", *args], cwd=cwd or self.repo, env=self.env,
            capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def commit(self, content):
        (self.repo / "source.txt").write_text(content + "\n", encoding="utf-8")
        self.git("add", "source.txt")
        self.git("commit", "--quiet", "-m", content)
        return self.git("rev-parse", "HEAD")

    def resolve(self, *args):
        return subprocess.run(
            ["sh", str(RESOLVER), *args], cwd=self.repo, env=self.env,
            capture_output=True, text=True, timeout=10, check=False,
        )

    def assert_resolves(self, tag, commit, *expected):
        result = self.resolve(tag, *expected)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, f"tag={tag}\ncommit={commit}\n")
        self.assertEqual(result.stderr, "")

    def assert_rejected(self, *args, message=None):
        result = self.resolve(*args)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertTrue(result.stderr)
        if message is not None:
            self.assertIn(message, result.stderr)

    def repository_files(self):
        return {
            str(path.relative_to(self.root)): path.read_bytes()
            for path in self.root.rglob("*") if path.is_file()
        }

    def test_lightweight_tag_returns_exact_output(self):
        self.git("tag", "v1.2.3")
        self.assert_resolves("v1.2.3", self.initial_commit, self.initial_commit)

    def test_requested_tag_is_preserved_when_versions_share_a_commit(self):
        self.git("tag", "v1.2.3")
        self.git("tag", "v1.2.4")
        # GoReleaser's default selection prefers the higher tag. Resolution
        # must retain the caller's tag, which the publishing action passes on.
        self.assertEqual(
            self.git("tag", "--points-at", "HEAD", "--sort", "-version:refname").splitlines(),
            ["v1.2.4", "v1.2.3"],
        )
        self.assert_resolves("v1.2.3", self.initial_commit, self.initial_commit)
        self.assert_resolves("v1.2.4", self.initial_commit, self.initial_commit)

    def test_annotated_tag_returns_peeled_commit(self):
        self.git("tag", "-a", "v1.2.3", "-m", "release")
        tag_object = self.git("rev-parse", "refs/tags/v1.2.3")
        self.assertNotEqual(tag_object, self.initial_commit)
        self.assert_resolves("v1.2.3", self.initial_commit, self.initial_commit)
        self.assert_rejected("v1.2.3", tag_object, message="expected commit")

    def test_strict_version_boundaries(self):
        for tag in ("v0.0.0", "v12.345.6789"):
            with self.subTest(tag=tag):
                self.git("tag", tag)
                self.assert_resolves(tag, self.initial_commit)

    def test_malformed_and_injection_inputs_are_rejected(self):
        self.git("tag", "v1.2.3")
        marker = self.root / "injected"
        tags = (
            "", "1.2.3", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2",
            "v1.2.3-rc.1", "v1.2.3+build", "refs/tags/v1.2.3", "--help",
            "v1.2.3\n", "v1.2.3\ncommit=" + self.initial_commit,
            "v1.2.3\r", " v1.2.3", "v1.2.3 ", "v1.2.3\t",
            f"v1.2.3;touch {marker}", f"v1.2.3$(touch {marker})",
            f"v1.2.3`touch {marker}`", "v1.2.3^{commit}",
        )
        for tag in tags:
            with self.subTest(tag=tag):
                self.assert_rejected(tag, message="malformed release tag")
        self.assertFalse(marker.exists())

    def test_unknown_tag_is_rejected(self):
        self.assert_rejected("v1.2.3", message="unknown release tag")

    def test_branch_with_same_name_is_not_a_release_tag(self):
        self.git("branch", "v1.2.3")
        self.assert_rejected("v1.2.3", message="unknown release tag")

    def test_tags_must_point_to_commits(self):
        tree = self.git("rev-parse", "HEAD^{tree}")
        self.git("tag", "v1.2.3", tree)
        self.git("tag", "-a", "v1.2.4", tree, "-m", "not a release commit")
        for tag in ("v1.2.3", "v1.2.4"):
            with self.subTest(tag=tag):
                self.assert_rejected(tag, message="does not resolve to a commit")

    def test_checkout_must_match_tag_commit(self):
        self.git("tag", "v1.2.3")
        self.commit("newer checkout")
        self.assert_rejected("v1.2.3", message="Checkout does not match")

    def test_expected_sha_must_match_tag_commit(self):
        self.git("tag", "v1.2.3")
        self.assert_rejected("v1.2.3", "0" * 40, message="expected commit")

    def test_expected_sha_requires_all_40_hexadecimal_characters(self):
        self.git("tag", "v1.2.3")
        marker = self.root / "injected"
        values = (
            "", self.initial_commit[:12], "a" * 39, "a" * 41, "g" * 40,
            "HEAD", "--help", self.initial_commit + "\n",
            f"$(touch {marker})", "a" * 39 + ";",
        )
        for expected in values:
            with self.subTest(expected=expected):
                self.assert_rejected("v1.2.3", expected, message="full 40-character")
        self.assertFalse(marker.exists())

    def test_expected_hexadecimal_sha_can_be_uppercase(self):
        self.git("tag", "v1.2.3")
        self.assert_resolves("v1.2.3", self.initial_commit, self.initial_commit.upper())

    def test_tag_outside_origin_main_is_rejected(self):
        self.git("checkout", "--quiet", "-b", "unmerged")
        self.commit("unmerged commit")
        self.git("tag", "v1.2.3")
        self.assert_rejected("v1.2.3", message="not on main")

    def test_origin_main_ref_is_required(self):
        self.git("tag", "v1.2.3")
        self.git("update-ref", "-d", "refs/remotes/origin/main")
        self.assert_rejected("v1.2.3", message="origin/main ref is required")

    def test_manual_older_tag_and_reruns_preserve_repository(self):
        self.git("tag", "-a", "v1.2.3", "-m", "older release")
        newer_commit = self.commit("newer main commit")
        self.git("push", "--quiet", "origin", "main")
        self.git("checkout", "--quiet", "--detach", "v1.2.3")
        self.assertEqual(self.git("rev-parse", "origin/main"), newer_commit)
        self.assertNotEqual(newer_commit, self.initial_commit)
        before = self.repository_files()
        self.assert_resolves("v1.2.3", self.initial_commit)
        self.assert_resolves("v1.2.3", self.initial_commit, self.initial_commit)
        self.assert_resolves("v1.2.3", self.initial_commit)
        self.assertEqual(self.repository_files(), before)

    def test_argument_count_is_checked(self):
        self.assert_rejected(message="Usage:")
        self.assert_rejected("v1.2.3", self.initial_commit, "extra", message="Usage:")


if __name__ == "__main__":
    unittest.main()
