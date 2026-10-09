"""Exercise the real Makefile installation in disposable directories."""

import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[1]
INPUTS = ("cmd", "internal", "scripts", "docs", "contrib", "go.mod", "go.sum", "README.md", "LICENSE")
DOC_FILES = (
    "README.md",
    "LICENSE",
    "docs/branding.md",
    "docs/agent-reporting.md",
    "docs/lifecycle-recovery.md",
    "docs/research/herdr-plugin-session-deletion.md",
    "docs/sway-session-plan.md",
    "docs/sway-session-verification.md",
    "docs/releasing.md",
    "docs/workflow_conventions.md",
    "docs/adr/0001-sqlite-session-runtime-state.md",
    "contrib/sway-session/config.toml",
    "contrib/herdr/config.toml",
    "contrib/codex/hooks.json",
    "contrib/apparmor/agent-home-guard",
)
COMPLETIONS = {
    "contrib/completions/bash/sway-session": "share/bash-completion/completions/sway-session",
    "contrib/completions/zsh/_sway-session": "share/zsh/site-functions/_sway-session",
    "contrib/completions/fish/sway-session.fish": "share/fish/vendor_completions.d/sway-session.fish",
}


class InstallTest(unittest.TestCase):
    def install(self, prefix_name, doc_name=None):
        with tempfile.TemporaryDirectory(prefix="sway-session-install-") as temporary:
            root = Path(temporary)
            build = root / "build"
            build.mkdir()
            # Read source inputs through symlinks; all build outputs and even
            # paths accidentally split by the shell stay in this private cwd.
            for name in INPUTS:
                (build / name).symlink_to(REPO / name)
            prefix = root / prefix_name
            doc_root = root / doc_name if doc_name else prefix / "share/doc/sway-session"
            command = ["make", "--no-print-directory", "-f", str(REPO / "Makefile"), "install", f"PREFIX={prefix}"]
            if doc_name:
                command.append(f"DOC_ROOT={doc_root}")
            env = os.environ.copy()
            # This is a fresh make invocation, not a recursive jobserver child.
            for key in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL"):
                env.pop(key, None)
            result = subprocess.run(command, cwd=build, env=env, capture_output=True, text=True, timeout=180)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

            expected = {prefix / "bin/sway-session": (build / "sway-session", 0o755)}
            for source, target in COMPLETIONS.items():
                expected[prefix / target] = (REPO / source, 0o644)
            for name in DOC_FILES:
                expected[doc_root / name] = (REPO / name, 0o644)
            expected[doc_root / "50-sway-session.conf"] = (REPO / "contrib/sway/50-sway-session.conf", 0o644)
            expected[doc_root / "scripts/verify-codex-boundary.sh"] = (REPO / "scripts/verify-codex-boundary.sh", 0o755)
            assets = list((REPO / "docs/assets").glob("*.jpeg"))
            self.assertTrue(assets, "The asset glob must install its matching source files")
            for source in assets:
                expected[doc_root / "docs/assets" / source.name] = (source, 0o644)

            destinations = [prefix] + ([doc_root] if doc_name else [])
            actual = {path for destination in destinations for path in destination.rglob("*") if path.is_file()}
            self.assertEqual(actual, set(expected))
            for target, (source, mode) in expected.items():
                self.assertEqual(target.read_bytes(), source.read_bytes(), str(target))
                self.assertEqual(stat.S_IMODE(target.stat().st_mode), mode, str(target))
            directories = {parent for target in expected for parent in target.parents if parent != root and root in parent.parents}
            actual_directories = {destination for destination in destinations}
            actual_directories.update(path for destination in destinations for path in destination.rglob("*") if path.is_dir())
            self.assertEqual(actual_directories, directories)
            for directory in actual_directories:
                self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o755, str(directory))
            self.assertEqual({path.name for path in build.iterdir()}, set(INPUTS) | {"sway-session"})
            self.assertEqual({path.name for path in root.iterdir()}, {"build", prefix_name} | ({doc_name} if doc_name else set()))

    def test_plain_prefix(self):
        self.install("prefix")

    def test_prefix_with_spaces(self):
        self.install("install prefix with spaces")

    def test_independent_doc_root_with_spaces(self):
        self.install("prefix", "separate documentation root")


if __name__ == "__main__":
    unittest.main()
