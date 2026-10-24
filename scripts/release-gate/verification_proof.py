"""Create CI evidence and conservatively reuse a successful exact-commit run.

Only canonical main-push CI artifacts are trusted. Lookup failures deliberately
produce empty outputs so callers run fresh verification. The token is never
written to disk or forwarded to an artifact download host.
"""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
import zlib


REPOSITORY = "marang/sway-session"
WORKFLOW = ".github/workflows/ci.yml"
SHA = re.compile(r"[0-9a-f]{40}\Z")
MAX_JSON = 2 * 1024 * 1024
MAX_ARCHIVE = 128 * 1024
MAX_PROOF = 16 * 1024
MAX_RUNS = 10
MAX_ARTIFACTS = 100


class Rejected(ValueError):
    """Evidence is absent, incompatible, or outside a trust boundary."""


def require(condition, message):
    if not condition:
        raise Rejected(message)


def positive_int(value):
    return type(value) is int and value > 0


def command(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL,
                                   timeout=30).strip()


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


def local_contract(commit):
    require(bool(SHA.fullmatch(commit)), "commit must be a full SHA")
    require(command("git", "rev-parse", "HEAD") == commit, "checkout differs from commit")
    subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    toolchain = pinned_toolchain(Path("go.mod").read_text())
    actual = command("go", "env", "GOVERSION")
    require(actual == toolchain, "actual Go toolchain differs from go.mod")
    paths = set(command("git", "ls-files", "scripts", ".github/workflows").splitlines())
    paths.update({"Makefile", "go.mod", "go.sum", ".goreleaser.yaml"})
    # Historical immutable releases predate the shared verifier. Hash their
    # actual source contract; their freshly staged tooling is recorded through
    # workflow_sha, which must equal commit before any later CI reuse.
    digest = hashlib.sha256()
    for name in sorted(paths):
        content = Path(name).read_bytes()
        digest.update(name.encode() + b"\0" + str(len(content)).encode() + b"\0" + content)
    return {"commit": commit, "contract_sha256": digest.hexdigest(),
            "toolchain": actual}


def create(commit, env):
    proof = {"schema": 1, **local_contract(commit)}
    repository = env.get("GITHUB_REPOSITORY", "")
    require(repository == REPOSITORY, "proof requires canonical repository")
    run_id = int(env.get("GITHUB_RUN_ID", "0"))
    attempt = int(env.get("GITHUB_RUN_ATTEMPT", "0"))
    require(positive_int(run_id) and positive_int(attempt), "invalid run identity")
    workflow_sha = env.get("GITHUB_WORKFLOW_SHA", "")
    require(bool(SHA.fullmatch(workflow_sha)), "missing workflow source SHA")
    proof.update(repository=repository, event=env.get("GITHUB_EVENT_NAME", ""),
                 ref=env.get("GITHUB_REF", ""), run_id=run_id, run_attempt=attempt,
                 workflow_sha=workflow_sha)
    return proof


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        old = urllib.parse.urlsplit(request.full_url)
        new = urllib.parse.urlsplit(newurl)
        require(new.scheme == "https" and not new.username and not new.password,
                "unsafe download redirect")
        redirected = super().redirect_request(request, fp, code, msg, headers, newurl)
        if (old.scheme, old.netloc) != (new.scheme, new.netloc):
            redirected.remove_header("Authorization")
        return redirected


class GitHub:
    def __init__(self, token):
        require(bool(token), "read token unavailable")
        self.token = token
        self.opener = urllib.request.build_opener(SafeRedirect())
        self.deadline = time.monotonic() + 60

    def read(self, path, limit):
        # Never follow URLs supplied in API payloads with the read token.
        require(path == f"/repos/{REPOSITORY}" or path.startswith(f"/repos/{REPOSITORY}/"),
                "unexpected API path")
        remaining = self.deadline - time.monotonic()
        require(remaining > 0, "lookup time budget exceeded")
        request = urllib.request.Request("https://api.github.com" + path, headers={
            "Authorization": "Bearer " + self.token,
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2026-03-10",
        })
        with self.opener.open(request, timeout=min(15, remaining)) as response:
            content = response.read(limit + 1)
        require(len(content) <= limit, "API response exceeds limit")
        return content

    def json(self, path):
        result = json.loads(self.read(path, MAX_JSON))
        require(isinstance(result, dict), "API response is not an object")
        return result

    def archive(self, artifact_id):
        return self.read(f"/repos/{REPOSITORY}/actions/artifacts/{artifact_id}/zip",
                         MAX_ARCHIVE)


def trusted_run(run, commit, repository_id, workflow_id):
    require(isinstance(run, dict), "invalid workflow run")
    require(run.get("status") == "completed" and run.get("conclusion") == "success",
            "CI has not succeeded")
    require(run.get("event") == "push" and run.get("head_branch") == "main",
            "CI must be a main push")
    require(run.get("head_sha") == commit and run.get("path") == WORKFLOW,
            "wrong commit or workflow")
    require(run.get("workflow_id") == workflow_id, "wrong workflow ID")
    require(positive_int(run.get("id")) and positive_int(run.get("run_attempt")),
            "invalid run identity")
    for key in ("repository", "head_repository"):
        repository = run.get(key)
        require(isinstance(repository, dict) and repository.get("id") == repository_id
                and repository.get("full_name") == REPOSITORY, "fork or wrong repository")


def decode_archive(content, artifact):
    digest = artifact.get("digest", "")
    require(isinstance(digest, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", digest),
            "artifact digest is missing")
    require(digest == "sha256:" + hashlib.sha256(content).hexdigest(),
            "artifact digest differs")
    with zipfile.ZipFile(io.BytesIO(content)) as archive:
        entries = archive.infolist()
        require(len(entries) == 1, "proof archive must contain one file")
        entry = entries[0]
        mode = entry.external_attr >> 16
        require(entry.filename == "verification-proof.json" and not entry.is_dir()
                and stat.S_IFMT(mode) in (0, stat.S_IFREG), "unexpected archive entry")
        require(0 < entry.file_size <= MAX_PROOF and entry.compress_size <= MAX_ARCHIVE
                and not entry.flag_bits & 1, "oversized or encrypted proof")
        with archive.open(entry) as source:
            data = source.read(MAX_PROOF + 1)
        require(len(data) <= MAX_PROOF, "proof exceeds limit")
    proof = json.loads(data)
    require(isinstance(proof, dict), "proof is not an object")
    return proof


def validate_proof(proof, expected, run):
    require(type(proof.get("schema")) is int and proof["schema"] == 1,
            "unsupported proof schema")
    for key, value in expected.items():
        require(proof.get(key) == value, "incompatible verification contract")
    require(proof.get("repository") == REPOSITORY and proof.get("event") == "push"
            and proof.get("ref") == "refs/heads/main", "wrong proof origin")
    require(type(proof.get("run_id")) is int and proof["run_id"] == run["id"]
            and type(proof.get("run_attempt")) is int
            and proof["run_attempt"] == run["run_attempt"], "stale run attempt")
    require(proof.get("workflow_sha") == expected["commit"], "wrong workflow source")


def select(commit, expected, api):
    prefix = f"/repos/{REPOSITORY}"
    repository = api.json(prefix)
    require(repository.get("full_name") == REPOSITORY and positive_int(repository.get("id")),
            "wrong canonical repository")
    repository_id = repository["id"]
    workflow = api.json(prefix + "/actions/workflows/ci.yml")
    require(workflow.get("path") == WORKFLOW and positive_int(workflow.get("id")),
            "wrong CI workflow")
    query = urllib.parse.urlencode({"head_sha": commit, "event": "push", "branch": "main",
                                    "status": "success", "per_page": MAX_RUNS})
    runs = api.json(prefix + "/actions/workflows/ci.yml/runs?" + query).get("workflow_runs")
    require(isinstance(runs, list) and len(runs) <= MAX_RUNS, "invalid workflow list")
    for candidate in runs:
        try:
            require(isinstance(candidate, dict) and positive_int(candidate.get("id")),
                    "invalid run candidate")
            run_path = prefix + f"/actions/runs/{candidate['id']}"
            run = api.json(run_path)
            trusted_run(run, commit, repository_id, workflow["id"])
            require(run["id"] == candidate["id"], "wrong run response")
            artifacts = api.json(run_path + f"/artifacts?per_page={MAX_ARTIFACTS}")
            count = artifacts.get("total_count")
            items = artifacts.get("artifacts")
            require(type(count) is int and 0 <= count <= MAX_ARTIFACTS
                    and isinstance(items, list) and len(items) == count,
                    "artifact list exceeds bound or is incomplete")
            matches = [item for item in items if isinstance(item, dict)
                       and item.get("name") == "verification-proof-" + commit]
            require(len(matches) == 1, "missing or duplicate proof artifact")
            artifact = matches[0]
            require(artifact.get("expired") is False and positive_int(artifact.get("id")),
                    "expired or invalid artifact")
            size = artifact.get("size_in_bytes")
            require(type(size) is int and 0 < size <= MAX_ARCHIVE, "oversized artifact")
            origin = artifact.get("workflow_run")
            require(isinstance(origin, dict) and origin.get("id") == run["id"]
                    and origin.get("repository_id") == repository_id
                    and origin.get("head_repository_id") == repository_id
                    and origin.get("head_sha") == commit and origin.get("head_branch") == "main",
                    "wrong artifact run origin")
            proof = decode_archive(api.archive(artifact["id"]), artifact)
            validate_proof(proof, expected, run)
            # A rerun can invalidate evidence between listing and downloading.
            current = api.json(run_path)
            trusted_run(current, commit, repository_id, workflow["id"])
            require(current["id"] == run["id"]
                    and current["run_attempt"] == run["run_attempt"], "CI attempt changed")
            return proof, {"commit": commit, "run_id": str(run["id"]),
                           "artifact_id": str(artifact["id"]), "origin": "ci-push-main"}
        except (Rejected, KeyError, TypeError, ValueError, OSError,
                urllib.error.URLError, zipfile.BadZipFile, RuntimeError, EOFError, zlib.error):
            # A bad candidate must never authorize reuse; another bounded
            # candidate may still contain a valid independently checked proof.
            continue
    return None, dict.fromkeys(("commit", "run_id", "artifact_id", "origin"), "")


def write_proof(path, proof):
    path.write_text(json.dumps(proof, sort_keys=True, indent=2) + "\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("create", "select"))
    parser.add_argument("--commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.operation == "create":
        try:
            write_proof(args.output, create(args.commit, os.environ))
        except (Rejected, ValueError, OSError, subprocess.SubprocessError):
            print("Cannot create verification proof: invalid checkout, toolchain or run context",
                  file=sys.stderr)
            return 1
        return 0
    outputs = dict.fromkeys(("commit", "run_id", "artifact_id", "origin"), "")
    # Remove a stale output before lookup; absent evidence cannot leave a proof.
    args.output.unlink(missing_ok=True)
    try:
        require(os.environ.get("GITHUB_REPOSITORY") == REPOSITORY, "noncanonical repository")
        expected = local_contract(args.commit)
        proof, outputs = select(args.commit, expected, GitHub(os.environ.get("GH_TOKEN", "")))
        if proof is not None:
            write_proof(args.output, proof)
    except (Rejected, ValueError, TypeError, KeyError, OSError, subprocess.SubprocessError,
            urllib.error.URLError, zipfile.BadZipFile, RuntimeError, EOFError, zlib.error):
        print("Compatible CI proof unavailable; fresh verification required", file=sys.stderr)
        outputs = dict.fromkeys(outputs, "")
    for key, value in outputs.items():
        print(f"{key}={value}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
