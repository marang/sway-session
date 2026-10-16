"""Standalone checks use actual package dependencies in disposable modules."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


CHECK = Path(__file__).resolve().parents[1] / "check-standalone-boundary.sh"
MODULE = "github.com/marang/sway-session"
ANIMATOR = "github.com/marang/sway-title-animator"


class StandaloneBoundaryTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sway-session-standalone-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.env = {
            "PATH": os.environ["PATH"], "LC_ALL": "C", "LANG": "C",
            "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local",
            "GOFLAGS": "-buildvcs=false",
            "GOPROXY": "off", "GOSUMDB": "off",
            "GOCACHE": str(self.root / "cache"),
            "GOPATH": str(self.root / "gopath"),
        }
        self.write("go.mod", f"module {MODULE}\n\ngo 1.26.0\n")
        self.write("cmd/sway-session/main.go", "package main\nfunc main() {}\n")
        for package in ("agentreport", "diagnostic", "herdrinit", "session",
                        "sessionrequest", "statefile", "swayipc", "titleindicator"):
            self.write(f"internal/{package}/package.go", f"package {package}\n")
        self.write("internal/titleindicator/testdata/v1.json", "{}\n")
        self.write("internal/titleindicator/wire_contract_test.go", "package titleindicator\n")

    def write(self, relative, content):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    def check_boundary(self):
        return subprocess.run(
            ["sh", str(CHECK)], cwd=self.root, env=self.env,
            capture_output=True, text=True, timeout=30, check=False,
        )

    def add_dependency(self, module, test_only=False):
        with (self.root / "go.mod").open("a", encoding="utf-8") as stream:
            stream.write(f"\nrequire {module} v0.0.0\nreplace {module} => ./dependency\n")
        self.write("dependency/go.mod", f"module {module}\n\ngo 1.26.0\n")
        self.write("dependency/adapter/package.go", "package adapter\n")
        name = "main_test.go" if test_only else "dependency.go"
        self.write(f"cmd/sway-session/{name}", f'package main\nimport _ "{module}/adapter"\n')

    def test_unrelated_hidden_checkout_is_outside_the_package_graph(self):
        self.write(".worktrees/unrelated-review/probe.go",
                   f'package probe\nimport _ "{ANIMATOR}"\n')
        result = self.check_boundary()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_production_animator_dependency_is_rejected(self):
        self.add_dependency(ANIMATOR)
        result = self.check_boundary()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("still imports the former combined module", result.stderr)
        self.assertIn(ANIMATOR + "/adapter", result.stdout)

    def test_test_only_animator_dependency_is_rejected(self):
        self.add_dependency(ANIMATOR, test_only=True)
        result = self.check_boundary()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("still imports the former combined module", result.stderr)
        self.assertIn(ANIMATOR + "/adapter", result.stdout)

    def test_similarly_named_module_is_not_the_former_module(self):
        self.add_dependency(ANIMATOR + "-helper")
        result = self.check_boundary()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
