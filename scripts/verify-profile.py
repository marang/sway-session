#!/usr/bin/env python3
"""Time the complete verification gate without caching or omitting test runs.

Go's JSON package start/pass events distinguish time before a test binary starts
(compilation, linking, implicit vet and scheduling) from its actual execution.
Raw test events and a structured profile stay outside the checkout.
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def run_step(target, command, output):
    output.mkdir(parents=True, exist_ok=True)
    is_test = command[:2] == ["go", "test"]
    if is_test:
        command = command[:2] + ["-json"] + command[2:]
    started = time.monotonic()
    packages = {}
    test_output = {}
    initial_start = None
    with (output / f"{target}.log").open("w", encoding="utf-8") as log:
        process = subprocess.Popen(command, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, text=True)
        for line in process.stdout:
            log.write(line)
            if not is_test:
                print(line, end="", flush=True)
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                print(line, end="", flush=True)
                continue
            package = event.get("Package")
            if not package:
                if "Output" in event:
                    print(event["Output"], end="", flush=True)
                continue
            if event.get("Action") == "start":
                wait = time.monotonic() - started
                initial_start = wait if initial_start is None else initial_start
                packages[package] = {"pre_execution_seconds": round(wait, 3)}
            if "Output" in event:
                test_output.setdefault(package, []).append(event["Output"])
            if event.get("Action") in {"pass", "fail", "skip"} and "Test" not in event:
                result = packages.setdefault(package, {})
                result.update(result=event["Action"],
                              execution_seconds=event.get("Elapsed", 0))
                print(f"{event['Action']}\t{package}\t{result['execution_seconds']:.3f}s",
                      flush=True)
                if event["Action"] == "fail":
                    print("".join(test_output.get(package, [])), end="", flush=True)
                test_output.pop(package, None)
        process.stdout.close()
        returncode = process.wait()
    result = {"target": target, "command": command, "exit_code": returncode,
              "wall_seconds": round(time.monotonic() - started, 3)}
    if is_test:
        result.update(initial_pre_execution_seconds=initial_start, packages=packages)
    (output / f"{target}.json").write_text(json.dumps(result, indent=2) + "\n",
                                         encoding="utf-8")
    print(f"VERIFY_PROFILE {target}: {result['wall_seconds']:.3f}s (exit {returncode})",
          flush=True)
    return returncode


def profile(output):
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("verification profile output directory must be empty")
    environment = dict(os.environ, VERIFY_PROFILE_DIR=str(output))
    # Historical release recovery may target a Makefile without the explicit
    # serial lanes. Run its complete gate serially rather than let heavy Go
    # commands compete; per-command wrappers are only available on new sources.
    jobs = 2 if "VERIFY_PROFILE_DIR" in Path("Makefile").read_text(encoding="utf-8") else 1
    started = time.monotonic()
    # Make's two recursive serial lanes bound concurrency to two commands.
    returncode = subprocess.call(
        ["make", f"--jobs={jobs}", "--output-sync=target", "verify"], env=environment)
    result = {"schema": 1, "wall_seconds": round(time.monotonic() - started, 3),
              "exit_code": returncode,
              "toolchain": subprocess.check_output(["go", "version"], text=True).strip(),
              "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
              "cpu_count": os.cpu_count(),
              "gomaxprocs": os.environ.get("GOMAXPROCS", "default"),
              "github_run_id": os.environ.get("GITHUB_RUN_ID"),
              "github_run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
              "jobs": jobs,
              "steps": [json.loads(path.read_text(encoding="utf-8"))
                        for path in sorted(output.glob("*.json"))
                        if path.name != "profile.json"]}
    (output / "profile.json").write_text(json.dumps(result, indent=2) + "\n",
                                        encoding="utf-8")
    summary = ["| Check | Wall seconds |", "| --- | ---: |"]
    summary.extend(f"| {step['target']} | {step['wall_seconds']:.3f} |"
                   for step in result["steps"])
    summary.append(f"| Complete make verify | {result['wall_seconds']:.3f} |")
    summary.extend(["", "| Test command | Before first execution (s) | Slowest package | Execution (s) |",
                    "| --- | ---: | --- | ---: |"])
    for step in result["steps"]:
        packages = step.get("packages", {})
        if packages:
            package, measurement = max(packages.items(), key=lambda item: item[1].get("execution_seconds", 0))
            summary.append(f"| {step['target']} | {step.get('initial_pre_execution_seconds')} | "
                           f"{package} | {measurement.get('execution_seconds')} |")
    summary.extend(["", "| Test command | Package | Before execution (s) | Execution (s) |",
                    "| --- | --- | ---: | ---: |"])
    for step in result["steps"]:
        for package, measurement in sorted(step.get("packages", {}).items()):
            wait = measurement.get("pre_execution_seconds")
            execution = measurement.get("execution_seconds")
            summary.append(f"| {step['target']} | {package} | "
                           f"{wait if wait is not None else 'unavailable'} | "
                           f"{execution if execution is not None else 'unavailable'} |")
    summary.extend(["", f"Toolchain: `{result['toolchain']}`", "",
                    f"Commit: `{result['commit']}`; CPUs: {result['cpu_count']}; "
                    f"GOMAXPROCS: `{result['gomaxprocs']}`; make jobs: {jobs}.", "",
                    "Tests use `-count=1`; race detection remains enabled. "
                    "Go JSON package events and pre-execution waits are in the artifact."])
    print("\n".join(summary), flush=True)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as handle:
            handle.write("\n".join(summary) + "\n")
    return returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--step")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    output = args.output.resolve()
    if args.step:
        command = args.command[1:] if args.command[:1] == ["--"] else args.command
        if not command:
            parser.error("--step requires a command after --")
        return run_step(args.step, command, output)
    if args.command:
        parser.error("commands require --step")
    return profile(output)


if __name__ == "__main__":
    sys.exit(main())
