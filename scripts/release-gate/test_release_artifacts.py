"""Artifact guards and create-only publication against private format fixtures."""

import copy
from contextlib import redirect_stdout
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest
import urllib.error
from unittest.mock import patch, MagicMock
import tarfile

SPEC = importlib.util.spec_from_file_location("release_artifacts", Path(__file__).with_name("release_artifacts.py"))
ARTIFACTS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ARTIFACTS)
SOURCE_IDENTITY = ARTIFACTS.source_identity
TAG = "v1.2.3"
COMMIT = "a" * 40
VERSION = "2.18.2"
TOOLCHAIN = "go1.26.5"
INPUTS = "b" * 64


def tar_bytes(files):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        for name, data in files.items():
            member = tarfile.TarInfo(name)
            member.size, member.mode = len(data), 0o755
            archive.addfile(member, io.BytesIO(data))
    return output.getvalue()


def executable(arch, tag=TAG, commit=COMMIT, toolchain=TOOLCHAIN):
    elf = bytearray(64)
    elf[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", elf, 18, ARTIFACTS.TARGETS[arch])
    header = bytearray(32)
    header[:14] = b"\xff Go buildinf:"
    header[14:16] = b"\x08\x02"
    return bytes(elf) + bytes(header) + bytes([len(toolchain)]) + toolchain.encode() + f"sway-session-build-v1|{tag[1:]}|{commit}|false|end-sway-session-build-v1".encode()


def deb_bytes(binary, arch, version="1.2.3", controls=None):
    control = f"Package: sway-session\nVersion: {version}\nArchitecture: {arch}\n".encode()
    if controls is None:
        controls = tar_bytes({"./control": control})
    output = b"!<arch>\n"
    for name, data in {"debian-binary": b"2.0\n", "control.tar.gz": controls, "data.tar.gz": tar_bytes({"./usr/bin/sway-session": binary})}.items():
        header = f"{name + '/':<16}{0:<12}{0:<6}{0:<6}{100644:<8}{len(data):<10}`\n".encode()
        output += header + data + (b"\n" if len(data) % 2 else b"")
    return output


def rpm_header(fields):
    indexes, strings = b"", b""
    for tag, value in fields.items():
        indexes += struct.pack(">IIII", tag, 6, len(strings), 1)
        strings += value.encode() + b"\0"
    return b"\x8e\xad\xe8\x01" + b"\0" * 4 + struct.pack(">II", len(fields), len(strings)) + indexes + strings


def cpio(name, data, mode=0o100755):
    fields = [1, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name) + 1, 0]
    result = b"070701" + b"".join(f"{value:08x}".encode() for value in fields) + name.encode() + b"\0"
    result += b"\0" * (-len(result) % 4)
    result += data
    return result + b"\0" * (-len(result) % 4)


def rpm_bytes(binary, arch, version="1.2.3"):
    signature = rpm_header({1000: "signature"})
    header = rpm_header({1000: "sway-session", 1001: version, 1022: {"amd64": "x86_64", "arm64": "aarch64"}[arch], 1125: "gzip"})
    lead = b"\xed\xab\xee\xdb" + b"\0" * 92
    result = lead + signature
    result += b"\0" * (-len(result) % 8)
    return result + header + gzip.compress(cpio("./usr/bin/sway-session", binary) + cpio("TRAILER!!!", b""))


class FakeGitHub:
    def __init__(self, assets=None, draft=False, exists=True):
        self.data = copy.deepcopy(assets or {})
        self.exists = exists
        self.draft = draft
        self.writes = []
        self.downloads = []
        self.race = None
        self.fail_upload = None

    def release(self, tag):
        if not self.exists:
            raise urllib.error.HTTPError("https://example.invalid", 404, "missing", {}, io.BytesIO())
        return {"id": 123, "tag_name": TAG, "draft": self.draft}

    def assets(self, release_id):
        return [{"id": index, "name": name, "state": "uploaded", "size": len(data), "digest": "sha256:" + ARTIFACTS.digest(data)} for index, (name, data) in enumerate(self.data.items())]

    def download(self, asset):
        self.downloads.append(asset["name"])
        return self.data[asset["name"]]

    def create(self, tag, commit, body):
        self.writes.append(("create", tag, commit))
        self.exists = True
        return self.release(tag)

    def upload(self, release_id, name, data):
        if self.race and self.race[0] == name:
            self.data[name] = self.race[1]
            raise urllib.error.HTTPError("https://example.invalid", 422, "duplicate", {}, io.BytesIO())
        self.writes.append(("upload", name))
        if self.fail_upload == name:
            raise urllib.error.HTTPError("https://example.invalid", 502, "upstream", {}, io.BytesIO())
        self.data[name] = data
        return next(asset for asset in self.assets(release_id) if asset["name"] == name)


class ArtifactGuardTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sway-artifact-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.dist, self.staged = self.root / "dist", self.root / "staged"
        self.dist.mkdir()
        self.source = patch.object(ARTIFACTS, "source_identity", return_value=(INPUTS, TOOLCHAIN))
        self.source.start()
        self.addCleanup(self.source.stop)
        for arch in ARTIFACTS.TARGETS:
            binary = executable(arch)
            for kind, data in {"tar.gz": tar_bytes({"sway-session": binary}), "deb": deb_bytes(binary, arch), "rpm": rpm_bytes(binary, arch)}.items():
                (self.dist / f"sway-session_1.2.3_linux_{arch}.{kind}").write_bytes(data)
        (self.dist / "CHANGELOG.md").write_text("Verified changes\n")
        self.write_checksums()

    def write_checksums(self):
        (self.dist / "checksums.txt").write_text("".join(f"{ARTIFACTS.digest(path.read_bytes())}  {path.name}\n" for path in sorted(self.dist.iterdir()) if path.name.endswith((".tar.gz", ".deb", ".rpm"))))

    def stage(self):
        return ARTIFACTS.stage(self.dist, self.staged, TAG, COMMIT, VERSION)

    def verify(self, tag=TAG, commit=COMMIT, version=VERSION):
        return ARTIFACTS.verify(self.staged, tag, commit, version)

    def expected(self):
        return {path.name: path.read_bytes() for path in self.staged.iterdir()}

    def publish(self, client):
        return ARTIFACTS.publish(self.staged, TAG, COMMIT, VERSION, client)

    def test_source_identity_requires_exact_clean_tracked_checkout(self):
        repo = self.root / "repo"
        repo.mkdir()
        def git(*args):
            return subprocess.check_output(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=Artifact fixture", "-c", "user.email=artifact@example.invalid", *args], cwd=repo, text=True, stderr=subprocess.PIPE).strip()
        git("init", "--initial-branch=main", "--template=", "--object-format=sha1")
        for name, text in {"go.mod": "module fixture\ngo 1.26.0\ntoolchain go1.26.5\n", "go.sum": "fixture\n", ".goreleaser.yaml": "version: 2\n"}.items():
            (repo / name).write_text(text)
        git("add", ".")
        git("commit", "-m", "Fixture")
        commit = git("rev-parse", "HEAD")
        with patch.object(ARTIFACTS, "command", side_effect=lambda executable, *args: git(*args)):
            original, toolchain = SOURCE_IDENTITY(commit)
            self.assertEqual(toolchain, TOOLCHAIN)
            self.assertRegex(original, r"^[0-9a-f]{64}$")
            with self.assertRaises(ValueError):
                SOURCE_IDENTITY("c" * 40)
            (repo / "untracked.go").write_text("package fixture\n")
            with self.assertRaises(ValueError):
                SOURCE_IDENTITY(commit)
            (repo / "untracked.go").unlink()
            (repo / "go.mod").write_text("modified\n")
            with self.assertRaises(ValueError):
                SOURCE_IDENTITY(commit)
            git("checkout", "--", "go.mod")
            self.assertEqual(SOURCE_IDENTITY(commit)[0], original)

    def test_signed_asset_download_never_forwards_token(self):
        opener = MagicMock()
        error = urllib.error.HTTPError("https://api.github.com", 302, "redirect", {"Location": "https://signed.example.invalid/asset"}, io.BytesIO())
        opener.open.side_effect = error
        with patch.object(ARTIFACTS.urllib.request, "build_opener", return_value=opener), patch.object(ARTIFACTS.urllib.request, "urlopen", return_value=io.BytesIO(b"asset")) as fetch:
            result = ARTIFACTS.GitHub("owner/repo", "secret-fixture-token").download({"id": 10})
        self.assertEqual(result, b"asset")
        self.assertEqual(opener.open.call_args.args[0].get_header("Authorization"), "Bearer secret-fixture-token")
        self.assertEqual(fetch.call_args.args, ("https://signed.example.invalid/asset",))

    def test_complete_manifest_checks_exact_package_set_and_binaries(self):
        manifest = self.stage()
        self.assertEqual(len(manifest["files"]), 8)
        self.assertEqual(manifest, self.verify())
        self.assertEqual(manifest["go_toolchain"], TOOLCHAIN)
        self.assertEqual(manifest["build_inputs_sha256"], INPUTS)
        for arch in ARTIFACTS.TARGETS:
            self.assertEqual(len({entry["binary_sha256"] for entry in manifest["files"] if entry["arch"] == arch}), 1)

    def test_wrong_expected_commit_tag_and_tool_version_reject_before_mutation(self):
        self.stage()
        before = self.expected()
        for field, value in [("commit", "c" * 40), ("tag", "v1.2.4"), ("version", "2.18.3")]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.verify(**{field: value})
        self.assertEqual(before, self.expected())

    def test_modified_source_digest_and_pinned_toolchain_reject(self):
        self.stage()
        for inputs, toolchain in [("c" * 64, TOOLCHAIN), (INPUTS, "go1.26.6")]:
            with self.subTest(toolchain=toolchain), patch.object(ARTIFACTS, "source_identity", return_value=(inputs, toolchain)), self.assertRaises(ValueError):
                self.verify()

    def test_tampered_files_manifest_fields_and_extra_assets_reject(self):
        self.stage()
        original = self.expected()
        for name in original:
            with self.subTest(name=name):
                path = self.staged / name
                path.write_bytes(original[name] + b"x")
                with self.assertRaises((ValueError, json.JSONDecodeError)):
                    self.verify()
                path.write_bytes(original[name])
        (self.staged / "surprise.sh").write_text("surprise")
        with self.assertRaises(ValueError):
            self.verify()

    def test_invalid_manifest_paths_duplicate_json_fields_and_target_matrix(self):
        self.stage()
        path = self.staged / ARTIFACTS.MANIFEST
        original = path.read_bytes()
        for mutation in [lambda m: m["files"][0].update(name="../outside"), lambda m: m.update(targets=["linux/amd64"]), lambda m: m.update(schema=True)]:
            manifest = json.loads(original)
            mutation(manifest)
            path.write_text(json.dumps(manifest))
            with self.assertRaises(ValueError):
                self.verify()
        path.write_bytes(original.replace(b'"schema": 1', b'"schema": 1, "schema": 1'))
        with self.assertRaises(ValueError):
            self.verify()

    def test_symlink_files_and_directory_reject(self):
        self.stage()
        target = self.staged / "CHANGELOG.md"
        target.unlink()
        target.symlink_to(self.dist / "CHANGELOG.md")
        with self.assertRaises(ValueError):
            self.verify()
        linked = self.root / "linked"
        linked.symlink_to(self.staged, target_is_directory=True)
        with self.assertRaises(ValueError):
            ARTIFACTS.verify(linked, TAG, COMMIT, VERSION)

    def test_stage_inspection_failure_does_not_create_output(self):
        (self.dist / "sway-session_1.2.3_linux_arm64.rpm").unlink()
        with self.assertRaises(ValueError):
            self.stage()
        self.assertFalse(self.staged.exists())

    def test_binary_identity_rejects_wrong_stamp_toolchain_architecture(self):
        for changes in [{"commit": "c" * 40}, {"tag": "v1.2.4"}, {"toolchain": "go1.26.6"}]:
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                ARTIFACTS.binary_identity(executable("amd64", **changes), TAG, COMMIT, "amd64", TOOLCHAIN)
        with self.assertRaises(ValueError):
            ARTIFACTS.binary_identity(executable("arm64"), TAG, COMMIT, "amd64", TOOLCHAIN)

    def test_deb_control_archive_entry_count_is_bounded(self):
        control = b"Package: sway-session\nVersion: 1.2.3\nArchitecture: amd64\n"
        files = {"./control": control}
        files.update({f"filler-{index}": b"" for index in range(ARTIFACTS.MAX_ENTRIES)})
        package = deb_bytes(executable("amd64"), "amd64", controls=tar_bytes(files))
        with self.assertRaisesRegex(ValueError, "Too many archive entries"):
            ARTIFACTS.deb_binary(package, "1.2.3", "amd64")

    def test_deb_control_archive_aggregate_expansion_is_bounded(self):
        control = b"Package: sway-session\nVersion: 1.2.3\nArchitecture: amd64\n"
        controls = tar_bytes({"./control": control, "filler": b"0" * (2 * 1024 * 1024)})
        package = deb_bytes(executable("amd64"), "amd64", controls=controls)
        self.assertLess(len(package), 1024 * 1024)
        with patch.object(ARTIFACTS, "MAX_FILE", 1024 * 1024), self.assertRaisesRegex(ValueError, "Oversized decompressed payload"):
            ARTIFACTS.deb_binary(package, "1.2.3", "amd64")

    def test_deb_control_oversized_declared_file_rejects_without_read_or_skip(self):
        member = tarfile.TarInfo("filler")
        member.size = 1024 * 1024 * 1024
        controls = gzip.compress(member.tobuf() + b"\0" * 1024)
        package = deb_bytes(executable("amd64"), "amd64", controls=controls)
        with patch.object(tarfile.TarFile, "extractfile", side_effect=AssertionError("must not extract oversized control payload")), self.assertRaisesRegex(ValueError, "Oversized archive"):
            ARTIFACTS.deb_binary(package, "1.2.3", "amd64")

    def test_packaged_binary_difference_wrong_package_metadata_and_unsafe_archive(self):
        cases = [
            ("deb", deb_bytes(executable("amd64") + b"different", "amd64")),
            ("deb", deb_bytes(executable("amd64"), "amd64", "1.2.4")),
            ("rpm", rpm_bytes(executable("amd64"), "amd64", "1.2.4")),
            ("tar.gz", tar_bytes({"../outside": b"unsafe", "sway-session": executable("amd64")})),
        ]
        for kind, data in cases:
            path = self.dist / f"sway-session_1.2.3_linux_amd64.{kind}"
            original = path.read_bytes()
            path.write_bytes(data)
            self.write_checksums()
            with self.subTest(kind=kind), self.assertRaises(ValueError):
                self.stage()
            self.assertFalse(self.staged.exists())
            path.write_bytes(original)
            self.write_checksums()

    def restore(self, client, **overrides):
        values = {"output": self.root / "restored", "tag": TAG, "commit": COMMIT, "goreleaser_version": VERSION}
        values.update(overrides)
        return ARTIFACTS.restore(client=client, **values)

    def test_restore_complete_matching_release_uses_only_reads(self):
        self.stage()
        expected = self.expected()
        client = FakeGitHub(expected)
        self.assertTrue(self.restore(client))
        restored = self.root / "restored"
        self.assertEqual({path.name: path.read_bytes() for path in restored.iterdir()}, expected)
        self.assertEqual(ARTIFACTS.verify(restored, TAG, COMMIT, VERSION), self.verify())
        self.assertEqual(client.writes, [])
        self.assertEqual(set(client.downloads), set(expected))
        self.assertFalse(any(path.name.startswith(".release-restore-") for path in self.root.iterdir()))

    def test_restore_404_falls_back_without_output_or_temporary_writes(self):
        before = set(self.root.iterdir())
        client = FakeGitHub(exists=False)
        self.assertFalse(self.restore(client))
        self.assertEqual(set(self.root.iterdir()), before)
        self.assertEqual(client.writes, [])
        self.assertEqual(client.downloads, [])

    def test_restore_partial_legacy_draft_and_unknown_asset_reject_before_downloads(self):
        self.stage()
        expected = self.expected()
        partial = dict(expected)
        partial.pop("checksums.txt")
        legacy = dict(expected)
        legacy.pop(ARTIFACTS.MANIFEST)
        unknown = dict(expected)
        unknown["surprise"] = unknown.pop("checksums.txt")
        before = set(self.root.iterdir())
        for client in [FakeGitHub(partial), FakeGitHub(legacy), FakeGitHub(unknown), FakeGitHub(expected, draft=True)]:
            with self.assertRaises(ValueError):
                self.restore(client)
            self.assertEqual(client.downloads, [])
            self.assertEqual(client.writes, [])
            self.assertEqual(set(self.root.iterdir()), before)

    def test_restore_corruption_wrong_manifest_and_commit_clean_up_temporaries(self):
        self.stage()
        expected = self.expected()
        wrong_manifest = dict(expected)
        wrong_manifest[ARTIFACTS.MANIFEST] = b"{}"
        corrupted = dict(expected)
        corrupted["checksums.txt"] += b"corrupt"
        before = set(self.root.iterdir())
        for contents, args in [(wrong_manifest, {}), (corrupted, {}), (expected, {"commit": "c" * 40}), (expected, {"goreleaser_version": "2.18.3"})]:
            client = FakeGitHub(contents)
            with self.assertRaises(ValueError):
                self.restore(client, **args)
            self.assertEqual(client.writes, [])
            self.assertEqual(set(self.root.iterdir()), before)
            self.assertFalse((self.root / "restored").exists())

    def test_restore_server_digest_mismatch_and_asset_size_bound(self):
        self.stage()
        client = FakeGitHub(self.expected())
        assets = client.assets(123)
        assets[0]["digest"] = "sha256:" + "c" * 64
        with patch.object(client, "assets", return_value=assets), self.assertRaises(ValueError):
            self.restore(client)
        self.assertFalse((self.root / "restored").exists())
        self.assertFalse(any(path.name.startswith(".release-restore-") for path in self.root.iterdir()))
        assets[0]["size"] = ARTIFACTS.MAX_FILE + 1
        client.downloads.clear()
        with patch.object(client, "assets", return_value=assets), self.assertRaises(ValueError):
            self.restore(client)
        self.assertEqual(client.downloads, [])
        self.assertEqual(client.writes, [])

    def test_restore_cli_outputs_only_github_output_assignment(self):
        client = FakeGitHub(exists=False)
        output = io.StringIO()
        args = ["release_artifacts.py", "restore", "--output", str(self.root / "restored"), "--tag", TAG, "--commit", COMMIT, "--goreleaser-version", VERSION, "--repo", "owner/repo"]
        with patch.object(ARTIFACTS.sys, "argv", args), patch.object(ARTIFACTS, "GitHub", return_value=client), redirect_stdout(output):
            self.assertEqual(ARTIFACTS.main(), 0)
        self.assertEqual(output.getvalue(), "restored=false\n")

    def test_existing_wrong_asset_digest_rejects_before_any_upload(self):
        self.stage()
        expected = self.expected()
        names = sorted(expected)
        assets = {names[0]: expected[names[0]], names[-1]: b"x" * len(expected[names[-1]])}
        client = FakeGitHub(assets)
        with self.assertRaises(ValueError):
            self.publish(client)
        self.assertEqual(client.writes, [])
        self.assertEqual(client.downloads, names[:1] + names[-1:])

    def test_matching_partial_release_uploads_only_missing_assets(self):
        self.stage()
        expected = self.expected()
        name = sorted(expected)[0]
        client = FakeGitHub({name: expected[name]})
        self.assertEqual(self.publish(client), 123)
        self.assertEqual(client.data, expected)
        self.assertEqual({write[1] for write in client.writes}, set(expected) - {name})
        self.assertTrue(all(write[0] == "upload" for write in client.writes))
        client.writes.clear()
        self.publish(client)
        self.assertEqual(client.writes, [])

    def test_draft_unknown_asset_and_invalid_local_manifest_fail_before_mutation(self):
        self.stage()
        for client in [FakeGitHub(draft=True), FakeGitHub({"unknown": b"x"})]:
            with self.assertRaises(ValueError):
                self.publish(client)
            self.assertEqual(client.writes, [])
        (self.staged / ARTIFACTS.MANIFEST).write_text("{}")
        client = FakeGitHub(exists=False)
        with self.assertRaises(ValueError):
            self.publish(client)
        self.assertEqual(client.writes, [])

    def test_new_release_only_created_after_verification(self):
        self.stage()
        client = FakeGitHub(exists=False)
        self.publish(client)
        self.assertEqual(client.writes[0], ("create", TAG, COMMIT))
        self.assertEqual(client.data, self.expected())

    def test_upload_422_race_compares_digest_without_overwriting(self):
        self.stage()
        name = sorted(self.expected())[0]
        for content, successful in [(self.expected()[name], True), (b"conflicting", False)]:
            client = FakeGitHub()
            client.race = (name, content)
            if successful:
                self.publish(client)
                self.assertEqual(client.data, self.expected())
            else:
                with self.assertRaises(ValueError):
                    self.publish(client)
                self.assertEqual(client.writes, [])
            self.assertNotIn(("upload", name), client.writes)

    def test_upstream_upload_failure_is_not_retried_or_deleted(self):
        self.stage()
        client = FakeGitHub()
        name = sorted(self.expected())[0]
        client.fail_upload = name
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.publish(client)
        caught.exception.close()
        self.assertEqual(client.writes, [("upload", name)])

    def test_size_bound_and_existing_server_digest_are_enforced(self):
        self.stage()
        with patch.object(ARTIFACTS, "MAX_FILE", 10), self.assertRaises(ValueError):
            self.verify()
        expected = self.expected()
        name = next(iter(expected))
        client = FakeGitHub({name: expected[name]})
        asset = client.assets(123)[0]
        asset["digest"] = "sha256:" + "c" * 64
        with self.assertRaises(ValueError):
            ARTIFACTS.check_existing(client, [asset], expected)
        self.assertEqual(client.writes, [])


if __name__ == "__main__":
    unittest.main()
