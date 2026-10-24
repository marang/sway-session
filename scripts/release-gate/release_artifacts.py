#!/usr/bin/env python3
"""Stage, verify, and upload already built release assets without rebuilding.

The manifest is integrity evidence, not independent authorization. Callers must
bind the immutable artifact ID to a successful trusted exact-commit workflow.
"""

import argparse
import gzip
import hashlib
import io
import json
import lzma
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import tarfile
import urllib.error
import urllib.parse
import urllib.request

MANIFEST = "release-manifest.json"
MAX_FILE = 256 * 1024 * 1024
MAX_JSON = 1024 * 1024
MAX_TOTAL = 512 * 1024 * 1024
MAX_ENTRIES = 4096
TARGETS = {"amd64": 62, "arm64": 183}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_file(path, limit=None):
    if limit is None:
        limit = MAX_FILE
    require(not path.is_symlink() and path.is_file(), f"Not a regular file: {path.name}")
    require(0 < path.stat().st_size <= limit, f"Invalid size: {path.name}")
    with path.open("rb") as stream:
        data = stream.read(limit + 1)
    require(len(data) <= limit, f"Oversized file: {path.name}")
    return data


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "Duplicate JSON key")
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs)


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def pinned_toolchain(module):
    explicit = re.search(r"^toolchain (go[0-9]+\.[0-9]+\.[0-9]+)\s*$", module, re.M)
    if explicit is not None:
        return explicit[1]
    # Never ignore an explicit but unsupported toolchain selection.
    require(re.search(r"^toolchain\b", module, re.M) is None,
            "Invalid explicit Go toolchain pin")
    minimum = re.search(r"^go ([0-9]+\.[0-9]+\.[0-9]+)\s*$", module, re.M)
    require(minimum is not None, "An exact Go patch version is required")
    # go mod tidy removes a redundant toolchain directive equal to this version.
    return "go" + minimum[1]


def source_identity(commit):
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "Invalid full commit")
    require(command("git", "rev-parse", "HEAD") == commit, "Checkout commit mismatch")
    require(not command("git", "diff", "--name-only", "HEAD"), "Modified build inputs")
    require(not command("git", "ls-files", "--others", "--exclude-standard", "--", "*.go"), "Untracked Go build inputs")
    root = Path(command("git", "rev-parse", "--show-toplevel"))
    inputs = {name: digest(read_file(root / name)) for name in ("go.mod", "go.sum", ".goreleaser.yaml")}
    inputs["source_tree"] = command("git", "rev-parse", "HEAD^{tree}")
    module = (root / "go.mod").read_text()
    return digest(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()), pinned_toolchain(module)


def expected_files(tag):
    require(re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag), "Invalid release tag")
    files = {f"sway-session_{tag[1:]}_linux_{arch}.{kind}": (kind, arch)
             for arch in TARGETS for kind in ("tar.gz", "deb", "rpm")}
    files.update({"checksums.txt": ("checksums", None), "CHANGELOG.md": ("changelog", None)})
    return files


def bounded_decompress(data, compressor):
    stream = gzip.GzipFile(fileobj=io.BytesIO(data)) if compressor is gzip.GzipFile else compressor(io.BytesIO(data))
    try:
        result = stream.read(MAX_FILE + 1)
    finally:
        stream.close()
    require(len(result) <= MAX_FILE, "Oversized decompressed payload")
    return result


def tar_binary(data, name, limit=None):
    if limit is None:
        limit = MAX_FILE
    # Bound the physical expansion too: tarfile can otherwise decompress and
    # skip enormous filler, padding, or extended headers during iteration.
    if data.startswith(b"\x1f\x8b"):
        data = bounded_decompress(data, gzip.GzipFile)
    elif data.startswith(b"\xfd7zXZ\x00"):
        data = bounded_decompress(data, lzma.LZMAFile)
    require(len(data) <= MAX_FILE, "Oversized decompressed tar payload")
    found = None
    total = 0
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        for index, member in enumerate(archive):
            require(index < MAX_ENTRIES, "Too many archive entries")
            total += max(member.size, 0)
            require(total <= MAX_FILE, "Oversized archive")
            normalized = member.name.removeprefix("./")
            require(not normalized.startswith("/") and ".." not in Path(normalized).parts, "Unsafe archive path")
            require(not member.issym() and not member.islnk() and (member.isfile() or member.isdir()), "Unsafe archive entry")
            if normalized == name:
                require(member.isfile() and found is None and member.size <= limit, "Duplicate, oversized, or invalid requested archive file")
                found = archive.extractfile(member).read(limit + 1)
    require(found is not None, f"Missing requested archive file: {name}")
    return found


def deb_binary(data, version, arch):
    require(data.startswith(b"!<arch>\n"), "Invalid DEB magic")
    members = {}
    offset = 8
    while offset < len(data):
        header = data[offset:offset + 60]
        require(len(header) == 60 and header[58:] == b"`\n", "Invalid DEB member")
        name = header[:16].decode("ascii").strip().rstrip("/")
        size = int(header[48:58])
        require(0 <= size <= MAX_FILE and offset + 60 + size <= len(data), "Invalid DEB size")
        require(name not in members, "Duplicate DEB member")
        members[name] = data[offset + 60:offset + 60 + size]
        offset += 60 + size + size % 2
    controls = [v for k, v in members.items() if k.startswith("control.tar")]
    payloads = [v for k, v in members.items() if k.startswith("data.tar")]
    require(len(controls) == len(payloads) == 1 and members.get("debian-binary") == b"2.0\n", "Invalid DEB layout")
    control = tar_binary(controls[0], "control", MAX_JSON).decode()
    fields = {}
    for line in control.splitlines():
        if ": " in line and not line.startswith(" "):
            key, value = line.split(": ", 1)
            require(key not in fields, "Duplicate DEB control field")
            fields[key] = value
    require(fields.get("Package") == "sway-session" and fields.get("Version") == version and fields.get("Architecture") == arch, "DEB identity mismatch")
    return tar_binary(payloads[0], "usr/bin/sway-session")


def rpm_header(data, offset):
    require(data[offset:offset + 4] == b"\x8e\xad\xe8\x01", "Invalid RPM header")
    count, size = struct.unpack_from(">II", data, offset + 8)
    require(count <= 4096 and size <= MAX_FILE, "Oversized RPM header")
    start = offset + 16 + count * 16
    end = start + size
    require(end <= len(data), "Truncated RPM header")
    values = {}
    for index in range(count):
        tag, kind, position, number = struct.unpack_from(">IIII", data, offset + 16 + index * 16)
        require(position < size and number <= MAX_FILE, "Invalid RPM index")
        if kind == 6:
            stop = data.find(b"\0", start + position, end)
            require(stop >= 0, "Invalid RPM string")
            require(tag not in values, "Duplicate RPM string tag")
            values[tag] = data[start + position:stop].decode("utf-8")
    return values, end


def rpm_binary(data, version, arch):
    require(data[:4] == b"\xed\xab\xee\xdb" and len(data) > 112, "Invalid RPM magic")
    _, end = rpm_header(data, 96)
    fields, end = rpm_header(data, (end + 7) // 8 * 8)
    require(fields.get(1000) == "sway-session" and fields.get(1001) == version and fields.get(1022) == {"amd64": "x86_64", "arm64": "aarch64"}[arch], "RPM identity mismatch")
    compressor = fields.get(1125, "gzip")
    require(compressor in ("gzip", "xz"), "Unsupported RPM compressor")
    payload = bounded_decompress(data[end:], gzip.GzipFile if compressor == "gzip" else lzma.LZMAFile)
    offset = 0
    found = None
    entries = 0
    while offset < len(payload):
        entries += 1
        require(entries <= MAX_ENTRIES, "Too many RPM entries")
        header = payload[offset:offset + 110]
        require(len(header) == 110 and header[:6] in (b"070701", b"070702"), "Invalid RPM cpio entry")
        mode = int(header[14:22], 16)
        size, namesize = int(header[54:62], 16), int(header[94:102], 16)
        require(0 < namesize <= 4096 and size <= MAX_FILE, "Invalid RPM cpio size")
        name_start = offset + 110
        name_end = name_start + namesize
        require(name_end <= len(payload) and payload[name_end - 1] == 0, "Invalid RPM cpio name")
        name = payload[name_start:name_end - 1].decode()
        body = (name_end + 3) // 4 * 4
        require(body + size <= len(payload), "Truncated RPM cpio content")
        if name == "TRAILER!!!":
            break
        normalized = name.removeprefix("./").removeprefix("/")
        require(".." not in Path(normalized).parts and mode & 0o170000 in (0o100000, 0o040000), "Unsafe RPM entry")
        if normalized == "usr/bin/sway-session":
            require(found is None and mode & 0o170000 == 0o100000, "Duplicate or invalid RPM binary")
            found = payload[body:body + size]
        offset = (body + size + 3) // 4 * 4
    require(found is not None, "Missing RPM binary")
    return found


def binary_identity(binary, tag, commit, arch, toolchain):
    require(binary[:4] == b"\x7fELF" and len(binary) >= 20 and binary[4:6] == b"\x02\x01", "Expected little-endian ELF64")
    require(struct.unpack_from("<H", binary, 18)[0] == TARGETS[arch], "Binary architecture mismatch")
    stamp = f"sway-session-build-v1|{tag[1:]}|{commit}|false|end-sway-session-build-v1".encode()
    stamps = re.findall(rb"sway-session-build-v1\|[^|]+\|[^|]+\|[^|]+\|end-sway-session-build-v1", binary)
    require(stamps and all(value == stamp for value in stamps), "Binary build identity mismatch")
    at = binary.find(b"\xff Go buildinf:")
    require(at >= 0 and len(binary) >= at + 32 and binary[at + 15] & 2, "Missing inline Go build information")
    position = at + 32
    length = shift = 0
    while True:
        require(position < len(binary) and shift <= 63, "Invalid Go build information")
        byte = binary[position]
        position += 1
        length |= (byte & 127) << shift
        if byte < 128:
            break
        shift += 7
    require(length <= 128 and binary[position:position + length].decode() == toolchain, "Binary Go toolchain mismatch")
    return digest(binary)


def inspect_files(directory, tag, commit, toolchain):
    result = []
    binaries = {}
    files = expected_files(tag)
    total = 0
    for name, (kind, arch) in sorted(files.items()):
        data = read_file(directory / name)
        total += len(data)
        require(total <= MAX_TOTAL, "Oversized release asset set")
        entry = {"name": name, "kind": kind, "arch": arch, "size": len(data), "sha256": digest(data)}
        if arch:
            if kind == "tar.gz":
                binary = tar_binary(data, "sway-session")
            elif kind == "deb":
                binary = deb_binary(data, tag[1:], arch)
            else:
                binary = rpm_binary(data, tag[1:], arch)
            entry["binary_sha256"] = binary_identity(binary, tag, commit, arch, toolchain)
            require(arch not in binaries or binaries[arch] == entry["binary_sha256"], "Packaged binaries differ")
            binaries[arch] = entry["binary_sha256"]
        result.append(entry)
    checksums = (directory / "checksums.txt").read_text().splitlines()
    expected = {e["name"]: e["sha256"] for e in result if e["arch"]}
    actual = {}
    for line in checksums:
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.-]+)", line)
        require(match and match[2] not in actual, "Invalid or duplicate checksum")
        actual[match[2]] = match[1]
    require(actual == expected, "Checksum asset set mismatch")
    return result


def stage(dist, output, tag, commit, goreleaser_version):
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", goreleaser_version), "Invalid GoReleaser version")
    inputs, toolchain = source_identity(commit)
    files = inspect_files(dist, tag, commit, toolchain)
    require(not output.exists(), "Staging directory already exists")
    manifest = {"schema": 1, "tag": tag, "commit": commit, "go_toolchain": toolchain,
                "goreleaser_version": goreleaser_version, "build_inputs_sha256": inputs,
                "targets": ["linux/amd64", "linux/arm64"], "files": files}
    output.mkdir(parents=True)
    try:
        for entry in files:
            shutil.copyfile(dist / entry["name"], output / entry["name"])
        (output / MANIFEST).write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")
        verify(output, tag, commit, goreleaser_version)
    except Exception:
        shutil.rmtree(output)
        raise
    return manifest


def verify(directory, tag, commit, goreleaser_version):
    require(directory.is_dir() and not directory.is_symlink(), "Invalid asset directory")
    inputs, toolchain = source_identity(commit)
    manifest = strict_json(read_file(directory / MANIFEST, MAX_JSON))
    require(set(manifest) == {"schema", "tag", "commit", "go_toolchain", "goreleaser_version", "build_inputs_sha256", "targets", "files"}, "Invalid manifest schema")
    require(type(manifest["schema"]) is int and manifest["schema"] == 1, "Unsupported manifest schema")
    require(manifest["tag"] == tag and manifest["commit"] == commit, "Manifest release identity mismatch")
    require(manifest["go_toolchain"] == toolchain and manifest["goreleaser_version"] == goreleaser_version and manifest["build_inputs_sha256"] == inputs, "Manifest build inputs mismatch")
    require(manifest["targets"] == ["linux/amd64", "linux/arm64"], "Manifest targets mismatch")
    require({p.name for p in directory.iterdir()} == set(expected_files(tag)) | {MANIFEST}, "Unexpected or missing staged asset")
    require(isinstance(manifest["files"], list) and len(manifest["files"]) == len(expected_files(tag)), "Invalid manifest file list")
    names = set()
    for entry in manifest["files"]:
        require(isinstance(entry, dict) and entry.get("name") in expected_files(tag) and entry["name"] not in names, "Invalid manifest file name")
        names.add(entry["name"])
        data = read_file(directory / entry["name"])
        require(type(entry.get("size")) is int and entry["size"] == len(data) and entry.get("sha256") == digest(data), "Manifest file digest mismatch")
    require(manifest["files"] == inspect_files(directory, tag, commit, toolchain), "Manifest file identity mismatch")
    return manifest


class GitHub:
    def __init__(self, repo, token):
        require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo), "Invalid repository")
        require(bool(token), "GH_TOKEN is required")
        self.repo, self.token = repo, token

    def request(self, method, path, data=None, raw=False):
        host = "https://uploads.github.com" if path.startswith("/upload/") else "https://api.github.com"
        path = path.removeprefix("/upload")
        headers = {"Authorization": f"Bearer {self.token}", "Accept": "application/octet-stream" if raw else "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
        if data is not None:
            headers["Content-Type"] = "application/octet-stream" if isinstance(data, bytes) else "application/json"
            if not isinstance(data, bytes):
                data = json.dumps(data).encode()
        request = urllib.request.Request(host + path, data=data, headers=headers, method=method)
        # Never send Authorization to a redirect host. Asset downloads use an
        # unauthenticated second request to the signed URL.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, response_headers, newurl):
                return None
        opener = urllib.request.build_opener(NoRedirect())
        try:
            response = opener.open(request, timeout=60)
        except urllib.error.HTTPError as error:
            if raw and error.code == 302:
                url = error.headers.get("Location", "")
                error.close()
                parsed = urllib.parse.urlparse(url)
                require(parsed.scheme == "https" and bool(parsed.hostname) and parsed.username is None and parsed.password is None, "Unsafe asset redirect")
                response = urllib.request.urlopen(url, timeout=60)
            else:
                raise
        with response:
            body = response.read((MAX_FILE if raw else MAX_JSON) + 1)
        require(len(body) <= (MAX_FILE if raw else MAX_JSON), "Oversized GitHub response")
        return body if raw else strict_json(body)

    def release(self, tag):
        return self.request("GET", f"/repos/{self.repo}/releases/tags/{tag}")

    def assets(self, release_id):
        # This project has nine expected assets; larger lists are rejected.
        data = self.request("GET", f"/repos/{self.repo}/releases/{release_id}/assets?per_page=100")
        require(isinstance(data, list) and len(data) < 100, "Unexpected asset list size")
        return data

    def download(self, asset):
        return self.request("GET", f"/repos/{self.repo}/releases/assets/{int(asset['id'])}", raw=True)

    def create(self, tag, commit, body):
        return self.request("POST", f"/repos/{self.repo}/releases", {"tag_name": tag, "target_commitish": commit, "name": tag, "body": body, "draft": False, "prerelease": False, "make_latest": "legacy"})

    def upload(self, release_id, name, data):
        return self.request("POST", f"/upload/repos/{self.repo}/releases/{release_id}/assets?name={urllib.parse.quote(name)}", data)


def check_existing(client, assets, expected):
    present = set()
    for asset in assets:
        name = asset.get("name")
        require(name in expected and name not in present, "Unexpected or duplicate published asset")
        data = expected[name]
        require(asset.get("state") == "uploaded" and asset.get("size") == len(data), "Existing asset state/size mismatch")
        downloaded = client.download(asset)
        require(len(downloaded) == len(data) and digest(downloaded) == digest(data), "Existing asset digest mismatch")
        if asset.get("digest"):
            require(asset["digest"] == "sha256:" + digest(data), "Existing server digest mismatch")
        present.add(name)
    return present


def publish(directory, tag, commit, goreleaser_version, client):
    manifest = verify(directory, tag, commit, goreleaser_version)
    expected = {e["name"]: read_file(directory / e["name"]) for e in manifest["files"]}
    expected[MANIFEST] = read_file(directory / MANIFEST, MAX_JSON)
    try:
        release = client.release(tag)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
        error.close()
        release = None
    if release is not None:
        require(release.get("tag_name") == tag and not release.get("draft"), "Existing draft or tag mismatch requires explicit recovery")
        present = check_existing(client, client.assets(release["id"]), expected)
    else:
        present = set()
        release = client.create(tag, commit, expected["CHANGELOG.md"].decode())
        require(release.get("tag_name") == tag and not release.get("draft"), "Created release identity mismatch")
    for name in sorted(expected):
        if name in present:
            continue
        try:
            uploaded = client.upload(release["id"], name, expected[name])
            check_existing(client, [uploaded], expected)
        except urllib.error.HTTPError as error:
            if error.code != 422:
                raise
            error.close()
            # Another publisher may have won the create-only upload race.
            # Compare every existing asset before continuing, never delete one.
            present = check_existing(client, client.assets(release["id"]), expected)
            require(name in present, "Upload conflict did not produce the expected asset")
    require(check_existing(client, client.assets(release["id"]), expected) == set(expected), "Incomplete published release")
    return release["id"]


def restore(output, tag, commit, goreleaser_version, client):
    """Recover a complete matching release using read-only API operations.

    Incomplete or historical releases cannot authorize a new build. Their
    original successful package job and immutable artifact remain the recovery
    source; rerun failed jobs instead of replacing already published bytes.
    """
    expected = set(expected_files(tag)) | {MANIFEST}
    source_identity(commit)
    require(not output.exists() and not output.is_symlink(), "Restore output already exists")
    require(output.parent.is_dir(), "Restore output parent must exist")
    try:
        release = client.release(tag)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
        error.close()
        return False
    require(release.get("tag_name") == tag and not release.get("draft"), "Existing draft or tag mismatch requires explicit recovery")
    assets = client.assets(release["id"])
    names = [asset.get("name") for asset in assets]
    require(len(names) == len(expected) and all(isinstance(name, str) for name in names) and set(names) == expected,
            "Incomplete or historical release; preserve its assets and rerun failed jobs in the original workflow run")
    total = 0
    for asset in assets:
        limit = MAX_JSON if asset["name"] == MANIFEST else MAX_FILE
        size = asset.get("size")
        require(asset.get("state") == "uploaded" and type(size) is int and 0 < size <= limit, "Invalid published asset state/size")
        total += size
        require(total <= MAX_TOTAL, "Oversized published release")
    # Keep downloads private and on the destination filesystem. Verification
    # completes before the directory becomes visible at the requested output.
    with tempfile.TemporaryDirectory(prefix=".release-restore-", dir=output.parent) as temporary:
        staged = Path(temporary) / "assets"
        staged.mkdir()
        for asset in assets:
            data = client.download(asset)
            require(len(data) == asset["size"], "Downloaded asset size mismatch")
            if asset.get("digest"):
                require(asset["digest"] == "sha256:" + digest(data), "Downloaded server digest mismatch")
            (staged / asset["name"]).write_bytes(data)
        verify(staged, tag, commit, goreleaser_version)
        require(not output.exists() and not output.is_symlink(), "Restore output appeared during verification")
        staged.rename(output)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="operation", required=True)
    for operation in ("stage", "verify", "publish", "restore"):
        child = subparsers.add_parser(operation)
        child.add_argument("--tag", required=True)
        child.add_argument("--commit", required=True)
        child.add_argument("--goreleaser-version", required=True)
        if operation in ("stage", "restore"):
            child.add_argument("--output", type=Path, required=True)
            if operation == "stage":
                child.add_argument("--dist", type=Path, required=True)
        else:
            child.add_argument("--directory", type=Path, required=True)
        if operation in ("publish", "restore"):
            child.add_argument("--repo", required=True)
    args = parser.parse_args()
    try:
        if args.operation == "stage":
            stage(args.dist, args.output, args.tag, args.commit, args.goreleaser_version)
        elif args.operation == "verify":
            verify(args.directory, args.tag, args.commit, args.goreleaser_version)
        elif args.operation == "restore":
            restored = restore(args.output, args.tag, args.commit, args.goreleaser_version, GitHub(args.repo, os.environ.get("GH_TOKEN", "")))
            print("restored=" + str(restored).lower())
            return 0
        else:
            publish(args.directory, args.tag, args.commit, args.goreleaser_version, GitHub(args.repo, os.environ.get("GH_TOKEN", "")))
    except (ValueError, OSError, EOFError, KeyError, TypeError, subprocess.CalledProcessError, tarfile.TarError, UnicodeError, struct.error) as error:
        # HTTP error strings omit request headers and credentials.
        print(f"Release artifact guard rejected: {error}", file=sys.stderr)
        return 1
    print(f"Release artifacts {args.operation} passed for {args.tag} at {args.commit}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
