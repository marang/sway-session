"""Exercise the production AUR recipe rewrite against moved-tag archives.

Only the validation/rewrite run blocks execute. Git and source archives are
disposable fixtures, and every curl invocation uses a private fake executable.
"""

import hashlib
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
TAG_SOURCE = 'source=("sway-session-$pkgver.tar.gz::$url/archive/refs/tags/v$pkgver.tar.gz")'
VERSION = "9.8.7"
REPOSITORY = "marang/sway-session"

FAKE_CURL = """import os
from pathlib import Path
import sys

url = sys.argv[-1]
with open(os.environ['FIXTURE_REQUESTS'], 'a', encoding='utf-8') as log:
    log.write(url + '\\n')
if url == os.environ['FIXTURE_COMMIT_URL']:
    archive = os.environ['FIXTURE_ARCHIVE_A']
elif url == os.environ['FIXTURE_TAG_URL']:
    archive = os.environ['FIXTURE_ARCHIVE_B']
else:
    print('Unexpected fixture archive URL: ' + url, file=sys.stderr)
    sys.exit(22)
sys.stdout.buffer.write(Path(archive).read_bytes())
"""


class AURSourceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sway-session-aur-source-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo = self.root / "checkout"
        self.bin = self.root / "bin"
        self.repo.mkdir()
        self.bin.mkdir()
        self.requests = self.root / "archive-requests.txt"
        self.template = (ROOT / "PKGBUILD").read_text(encoding="utf-8")
        workflow = yaml.safe_load((ROOT / ".github/workflows/publish-aur.yml").read_text(encoding="utf-8"))
        steps = workflow["jobs"]["build"]["steps"]
        self.rewrite = next(step["run"] for step in steps if step["name"] == "Bump PKGBUILD version and checksum")
        self.validate = next(step["run"] for step in steps if step["name"] == "Validate release tag and main ancestry")
        # No inherited Git routing, configuration, hooks, credentials, or shell
        # startup hooks. All commands operate inside this disposable repository.
        self.env = {
            "PATH": str(self.bin) + os.pathsep + "/usr/bin:/bin",
            "LC_ALL": "C", "LANG": "C",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_ALLOW_PROTOCOL": "file",
            "GIT_AUTHOR_NAME": "AUR Source Test",
            "GIT_AUTHOR_EMAIL": "aur-source@example.invalid",
            "GIT_COMMITTER_NAME": "AUR Source Test",
            "GIT_COMMITTER_EMAIL": "aur-source@example.invalid",
            "GIT_AUTHOR_DATE": "2026-01-01T00:00:00+00:00",
            "GIT_COMMITTER_DATE": "2026-01-01T00:00:00+00:00",
            "GITHUB_WORKSPACE": str(self.repo),
            "GITHUB_REPOSITORY": REPOSITORY,
            "RELEASE_VERSION": VERSION,
            "VERSION": "v" + VERSION,
            "FIXTURE_REQUESTS": str(self.requests),
        }
        self.git("init", "--initial-branch=main", "--object-format=sha1", "--template=", ".")
        (self.repo / "PKGBUILD").write_text(self.template, encoding="utf-8")
        (self.repo / "identity.txt").write_text("verified source A\n", encoding="utf-8")
        self.git("add", "PKGBUILD", "identity.txt")
        self.git("commit", "--quiet", "-m", "verified source A")
        self.commit_a = self.git("rev-parse", "HEAD")
        self.git("tag", "v" + VERSION)
        (self.repo / "identity.txt").write_text("moved tag source B\n", encoding="utf-8")
        self.git("add", "identity.txt")
        self.git("commit", "--quiet", "-m", "moved tag source B")
        self.commit_b = self.git("rev-parse", "HEAD")
        self.git("update-ref", "refs/remotes/origin/main", self.commit_b)
        self.git("checkout", "--quiet", "--detach", self.commit_a)
        self.env["EXPECTED_COMMIT"] = self.commit_a
        self.url_a = f"https://github.com/{REPOSITORY}/archive/{self.commit_a}.tar.gz"
        self.url_tag = f"https://github.com/{REPOSITORY}/archive/refs/tags/v{VERSION}.tar.gz"
        archive_a = self.root / "source-a.tar.gz"
        archive_b = self.root / "source-b.tar.gz"
        self.archive(archive_a, self.commit_a, b"verified source A\n")
        self.archive(archive_b, self.commit_b, b"moved tag source B\n")
        self.sum_a = hashlib.sha256(archive_a.read_bytes()).hexdigest()
        self.sum_b = hashlib.sha256(archive_b.read_bytes()).hexdigest()
        self.assertNotEqual(self.sum_a, self.sum_b)
        self.env.update({
            "FIXTURE_COMMIT_URL": self.url_a,
            "FIXTURE_TAG_URL": self.url_tag,
            "FIXTURE_ARCHIVE_A": str(archive_a),
            "FIXTURE_ARCHIVE_B": str(archive_b),
        })
        fake_curl = self.bin / "curl"
        fake_curl.write_text("#!" + sys.executable + "\n" + FAKE_CURL, encoding="utf-8")
        fake_curl.chmod(0o700)

    def git(self, *args):
        result = subprocess.run(
            ["git", *args], cwd=self.repo, env=self.env,
            capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def archive(self, path, commit, content):
        with tarfile.open(path, "w:gz") as archive:
            member = tarfile.TarInfo(f"sway-session-{commit}/identity.txt")
            member.size = len(content)
            member.mtime = 0
            archive.addfile(member, io.BytesIO(content))

    def shell(self, script):
        return subprocess.run(
            ["/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script],
            cwd=self.repo, env=self.env, capture_output=True, text=True,
            timeout=10, check=False,
        )

    def requested_urls(self):
        return self.requests.read_text(encoding="utf-8").splitlines() if self.requests.exists() else []

    def metadata(self):
        result = self.shell(
            'source ./PKGBUILD\n'
            'printf "pkgver=%s\\npkgrel=%s\\ncommit=%s\\nsource=%s\\nchecksum=%s\\nldflags=%s\\n" '
            '"$pkgver" "$pkgrel" "${_commit-}" "${source[0]}" "${sha256sums[0]}" "${_go_ldflags[*]}"'
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return dict(line.split("=", 1) for line in result.stdout.splitlines())

    def tag_template(self):
        recipe = re.sub(r"^source=.*$", TAG_SOURCE, self.template, flags=re.MULTILINE)
        return re.sub(
            r'(\bcd "sway-session-)(?:\$pkgver|[0-9a-f]{40})(")',
            r"\1$pkgver\2", recipe,
        )

    def assert_immutable_recipe(self, template, has_commit=True):
        (self.repo / "PKGBUILD").write_text(template, encoding="utf-8")
        result = self.shell(self.validate)
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.shell(self.rewrite)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.requested_urls(), [self.url_a])
        state = self.metadata()
        self.assertEqual(state["pkgver"], VERSION)
        self.assertEqual(state["pkgrel"], "1")
        self.assertEqual(state["commit"], self.commit_a if has_commit else "")
        self.assertEqual(state["source"], f"sway-session-{VERSION}.tar.gz::{self.url_a}")
        self.assertEqual(state["checksum"], self.sum_a)
        self.assertNotEqual(state["checksum"], self.sum_b)
        recipe = (self.repo / "PKGBUILD").read_text(encoding="utf-8")
        directories = re.findall(r'^\s*cd "([^"]+)"$', recipe, flags=re.MULTILINE)
        self.assertEqual(directories, [f"sway-session-{self.commit_a}"] * 3)
        self.assertNotIn("/archive/refs/tags/", state["source"])
        self.assertNotIn("SKIP", recipe)
        if has_commit:
            self.assertIn(f"main.commit={self.commit_a}", state["ldflags"])
            self.assertIn(f"sway-session-build-v1|{VERSION}|{self.commit_a}|false|", state["ldflags"])

    def test_current_recipe_pins_download_source_checksum_and_all_three_directories(self):
        self.assert_immutable_recipe(self.template)

    def test_original_tag_recipe_does_not_download_a_moved_tag_archive(self):
        self.assert_immutable_recipe(self.tag_template())

    def test_literal_commit_recipe_is_rebound_to_the_verified_commit(self):
        stale_commit = "c" * 40
        recipe = self.tag_template().replace(
            TAG_SOURCE, f'source=("sway-session-$pkgver.tar.gz::$url/archive/{stale_commit}.tar.gz")',
        ).replace('cd "sway-session-$pkgver"', f'cd "sway-session-{stale_commit}"')
        self.assert_immutable_recipe(recipe)

    def test_older_recipe_without_build_commit_metadata_remains_usable(self):
        recipe = re.sub(r"^_commit=.*\n", "", self.tag_template(), flags=re.MULTILINE)
        recipe = re.sub(r"^_go_ldflags=.*$", "_go_ldflags=(-s -w -buildid=)", recipe, flags=re.MULTILINE)
        self.assertNotIn("_commit", recipe)
        self.assert_immutable_recipe(recipe, has_commit=False)

    def test_unsupported_recipe_forms_are_rejected_before_archive_download(self):
        tag_recipe = self.tag_template()
        unsupported = {
            "mutable branch": tag_recipe.replace("refs/tags/v$pkgver", "refs/heads/main"),
            "additional source": tag_recipe.replace(TAG_SOURCE, TAG_SOURCE[:-1] + ' "other-source.tar.gz")'),
            "missing source": tag_recipe.replace(TAG_SOURCE + "\n", ""),
            "unsupported directory": tag_recipe.replace('cd "sway-session-$pkgver"', 'cd "other-source"', 1),
        }
        for name, recipe in unsupported.items():
            with self.subTest(template=name):
                (self.repo / "PKGBUILD").write_text(recipe, encoding="utf-8")
                result = self.shell(self.rewrite)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unsupported package source", result.stderr)
                self.assertEqual(self.requested_urls(), [])

    def test_wrong_verified_commit_is_rejected_before_recipe_rewrite(self):
        self.env["EXPECTED_COMMIT"] = self.commit_b
        before = (self.repo / "PKGBUILD").read_bytes()
        result = self.shell(self.validate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Release tag changed after verification", result.stderr)
        self.assertEqual(self.requested_urls(), [])
        self.assertEqual((self.repo / "PKGBUILD").read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
