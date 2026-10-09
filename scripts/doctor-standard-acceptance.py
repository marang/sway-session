#!/usr/bin/env python3
"""Opt-in LAB-326 executable acceptance; all files/processes stay private.

Build the candidate outside the checkout, then pass its absolute path. Requires
Linux, Python 3 and Sway. The startup fixture uses an inert executable named
sway-session, never the candidate daemon/restore. No workstation socket or
configuration is inherited. Logs remain below /tmp only with --keep.
"""

import argparse
import codecs
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import re
import select
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import time
import unicodedata


HEADER = ("# Managed by sway-session doctor. Manual edits disable automatic repair.\n"
          "# sway-session-doctor-format: 1\n")
BASE = ("xwayland disable\noutput HEADLESS-1 mode 1280x720\n"
        "workspace 98 output HEADLESS-1\nworkspace 98\nset $mod Mod4\n")
DYNAMIC = ('set $wallpaper /unused-wallpapers\n'
           'exec /usr/bin/true "$(/usr/bin/true)" $wallpaper\n'
           'exec /bin/sh -c "/usr/bin/true; /usr/bin/true"\n')


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def shown(pages, text):
    # The product uses hard wrapping, including inside words. Compare displayed
    # cells without whitespace; frame borders are removed by DoctorPTY.scroll.
    return "".join(text.split()) in "".join(pages.split())


def write(path, content, mode=0o600):
    path.write_text(content)
    path.chmod(mode)


def snippet(executable, shortcuts=False):
    text = HEADER + f"exec --no-startup-id {executable} daemon\n"
    text += f"exec --no-startup-id {executable} restore\n"
    if shortcuts:
        text += f"bindsym $mod+Return exec --no-startup-id {executable} terminal --new\n"
        text += f"bindsym $mod+Shift+Return exec --no-startup-id {executable} terminal --ephemeral\n"
    return text


def until(predicate, description, seconds=8):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.025)
    raise AssertionError(f"timeout: {description}")


class PrivateSway:
    def __init__(self, root, env, config):
        self.root, self.env = root, env
        self.log = (root / "sway.log").open("wb")
        validate = subprocess.run(["/usr/bin/sway", "-C", "--config", str(config)],
                                  env=env, capture_output=True, timeout=10)
        require(validate.returncode == 0 and b"Error on line" not in validate.stderr,
                f"private Sway validation failed: {validate.stderr.decode(errors='replace')}")
        self.process = subprocess.Popen(["/usr/bin/sway", "--config", str(config)],
                                        env=env, cwd=root, stdout=self.log,
                                        stderr=self.log, start_new_session=True)
        self.socket = root / "run" / f"sway-ipc.{os.getuid()}.{self.process.pid}.sock"
        try:
            until(lambda: self.socket.exists() or self.process.poll() is not None,
                  "private Sway socket")
            require(self.process.poll() is None, "private Sway exited; inspect sway.log")
            tree = self.ipc(4)
            workspaces = self.ipc(1)
            require(tree["type"] == "root" and any(w["name"] == "98" for w in workspaces),
                    "private Sway fixture did not select workspace 98")
        except BaseException:
            self.close()
            raise

    def ipc(self, kind, payload=""):
        with socket.socket(socket.AF_UNIX) as conn:
            conn.settimeout(3)
            conn.connect(str(self.socket))
            encoded = payload.encode()
            conn.sendall(b"i3-ipc" + struct.pack("<II", len(encoded), kind) + encoded)

            def read(size):
                data = b""
                while len(data) < size:
                    part = conn.recv(size - len(data))
                    require(bool(part), "private Sway response ended early")
                    data += part
                return data

            header = read(14)
            require(header[:6] == b"i3-ipc", "invalid private IPC magic")
            size, _ = struct.unpack("<II", header[6:])
            require(size <= 4 << 20, "private IPC response exceeds bound")
            return json.loads(read(size))

    def reload(self):
        require(all(item["success"] for item in self.ipc(0, "reload")),
                "private reload failed")

    def close(self):
        if self.process.poll() is None:
            try:
                self.ipc(0, "exit")
            except (OSError, AssertionError):
                pass
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGTERM)
                try:
                    self.process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(self.process.pid, signal.SIGKILL)
                    self.process.wait(timeout=2)
        self.log.close()


class Screen:
    """Small VT screen observer for the CSI sequences emitted by Bubble Tea.

    Captures actual displayed cells after cursor-positioned differential frames,
    so paged detail evidence does not depend on raw escape-stripped fragments.
    """
    def __init__(self, width, height):
        self.width, self.height = width, height
        self.cells = [[" "] * width for _ in range(height)]
        self.row = self.col = 0
        self.pending = ""
        self.decoder = codecs.getincrementaldecoder("utf-8")("replace")

    def feed(self, data):
        self.pending += self.decoder.decode(data)
        while self.pending:
            if self.pending.startswith("\x1b["):
                match = re.match(r"\x1b\[([0-9;:?<>=]*)([ -/]*)([@-~])", self.pending)
                if not match:
                    break
                self.pending = self.pending[match.end():]
                args, _, final = match.groups()
                values = [int(value or 0) for value in args.split(";")] if re.fullmatch(r"[0-9;]*", args) else []
                first = values[0] if values else 0
                if final in "Hf":
                    self.row = max(0, (first or 1) - 1)
                    self.col = max(0, ((values[1] if len(values) > 1 else 1) or 1) - 1)
                elif final == "G":
                    self.col = max(0, (first or 1) - 1)
                elif final == "A":
                    self.row = max(0, self.row - (first or 1))
                elif final == "B":
                    self.row += first or 1
                elif final == "C":
                    self.col += first or 1
                elif final == "D":
                    self.col = max(0, self.col - (first or 1))
                elif final == "J":
                    if first == 2:
                        self.cells = [[" "] * self.width for _ in range(self.height)]
                    elif first == 0:
                        if self.row < self.height:
                            self.cells[self.row][self.col:] = [" "] * max(0, self.width - self.col)
                        for row in range(self.row + 1, self.height):
                            self.cells[row] = [" "] * self.width
                elif final == "K" and self.row < self.height:
                    start, end = (0, self.width) if first == 2 else ((0, self.col + 1) if first == 1 else (self.col, self.width))
                    self.cells[self.row][start:end] = [" "] * max(0, end - start)
                elif final == "X" and self.row < self.height:
                    end = min(self.width, self.col + (first or 1))
                    self.cells[self.row][self.col:end] = [" "] * max(0, end - self.col)
                continue
            if self.pending.startswith("\x1b]"):
                end = re.search(r"\x07|\x1b\\", self.pending)
                if not end:
                    break
                self.pending = self.pending[end.end():]
                continue
            if self.pending.startswith("\x1b"):
                if len(self.pending) < 2:
                    break
                self.pending = self.pending[2:]
                continue
            char, self.pending = self.pending[0], self.pending[1:]
            if char == "\r":
                self.col = 0
            elif char == "\n":
                self.row += 1
            elif char == "\b":
                self.col = max(0, self.col - 1)
            elif char >= " " and char != "\x7f":
                if self.col >= self.width:
                    self.col, self.row = 0, self.row + 1
                if self.row >= self.height:
                    self.cells.pop(0)
                    self.cells.append([" "] * self.width)
                    self.row = self.height - 1
                if unicodedata.combining(char):
                    continue
                self.cells[self.row][self.col] = char
                self.col += 2 if unicodedata.east_asian_width(char) in "WF" else 1

    def text(self):
        return "\n".join("".join(row).rstrip() for row in self.cells)


class DoctorPTY:
    def __init__(self, args, env, width, height, log):
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
        self.process = subprocess.Popen(args, env=env, stdin=slave, stdout=slave,
                                        stderr=slave, start_new_session=True)
        os.close(slave)
        self.screen = Screen(width, height)
        self.log = log.open("wb")
        self.pages = []
        try:
            self.wait("Setup doctor")
            self.settle(0.8)
        except BaseException:
            self.close()
            raise

    def settle(self, seconds=0.15):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            ready, _, _ = select.select([self.master], [], [], min(0.025, max(0, deadline - time.monotonic())))
            if ready:
                try:
                    data = os.read(self.master, 65536)
                except OSError:
                    break
                if not data:
                    break
                self.log.write(data)
                self.screen.feed(data)
                if b"\x1b[6n" in data:
                    os.write(self.master, b"\x1b[1;1R")
        self.pages.append(self.screen.text())

    def wait(self, text, seconds=8):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            self.settle(0.1)
            if text in self.screen.text():
                return
            require(self.process.poll() is None, f"TUI exited while waiting for {text}")
        raise AssertionError(f"TUI timeout waiting for {text}:\n{self.screen.text()}")

    def send(self, key):
        os.write(self.master, key.encode())
        self.settle()

    def scroll(self, pages=30):
        for _ in range(pages):
            previous = self.screen.text()
            self.send("\x1b[6~")
            if self.screen.text() == previous:
                break
        return " ".join(" ".join(page.replace("│", " ").split()) for page in self.pages)

    def select_integration(self):
        self.send("/")
        self.send("sway.integration")
        self.send("\r")

    def close(self):
        if self.process.poll() is None:
            self.send("q")
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGTERM)
                try:
                    self.process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(self.process.pid, signal.SIGKILL)
                    self.process.wait(timeout=2)
        self.log.close()
        os.close(self.master)


class Acceptance:
    def __init__(self, binary, root):
        self.binary, self.root, self.records = binary, root, []
        self.env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "TERM": "xterm-256color",
                    "NO_COLOR": "1", "HOME": str(root), "WLR_BACKENDS": "headless",
                    "WLR_HEADLESS_OUTPUTS": "1", "WLR_RENDERER": "pixman",
                    "LIBGL_ALWAYS_SOFTWARE": "1"}
        for key, name in (("XDG_RUNTIME_DIR", "run"), ("XDG_CONFIG_HOME", "config"),
                          ("XDG_STATE_HOME", "state"), ("XDG_CACHE_HOME", "cache"),
                          ("XDG_DATA_HOME", "data")):
            directory = root / name
            directory.mkdir(mode=0o700)
            self.env[key] = str(directory)
        self.env["HERDR_CONFIG_PATH"] = str(root / "herdr.toml")
        (root / "config" / "herdr").mkdir(mode=0o700)
        write(root / "herdr.toml", "[experimental]\npane_history = true\n")
        write(root / "session.toml", 'version = 2\n[terminal]\nadapter = "alacritty"\nsession_manager = "herdr"\n')
        config = root / "config" / "sway.conf"
        self.reload_calls = root / "reload-calls"
        observer = root / "reload-observer"
        write(observer, f"#!/bin/sh\nprintf 'load\\n' >> '{self.reload_calls}'\n", 0o700)
        write(config, BASE + f"exec_always {observer}\n")
        self.sway = PrivateSway(root, self.env, config)
        self.env["SWAYSOCK"] = str(self.sway.socket)
        until(lambda: self.reload_calls.exists(), "private reload observer startup")

    def fixture(self, name, profile=None, include="direct", dynamic=True):
        directory = self.root / name
        directory.mkdir(mode=0o700)
        config, owned = directory / "config", directory / "50-sway-session-doctor.conf"
        text = BASE + (DYNAMIC if dynamic else "")
        if profile is not None:
            write(owned, profile)
            if include == "indirect":
                intermediary = directory / "indirect.conf"
                write(intermediary, f'include "{owned}"\n')
                text += f'include "{intermediary}"\n'
            else:
                text += f'include "{owned}"\n' * (2 if include == "repeated" else 1)
        write(config, text)
        return config, owned

    def args(self, config, *extra):
        return [str(self.binary), "--config", str(self.root / "session.toml"),
                "doctor", "--socket", str(self.sway.socket), "--sway-config", str(config), *extra]

    def cli(self, config, *extra, structured=True):
        args = self.args(config, *extra)
        if structured:
            args.insert(1, "--json")
        result = subprocess.run(args, env=self.env, capture_output=True, timeout=10)
        if structured:
            payload = result.stdout or result.stderr
            require(bool(payload), "CLI returned no JSON")
            return result.returncode, json.loads(payload)
        return result.returncode, result.stdout.decode()

    def check(self, config, expected, fix=True):
        _, result = self.cli(config, "--check")
        require(result["version"] == 1, "JSON envelope version changed")
        check = next(item for item in result["doctor"]["checks"] if item["id"] == "sway.integration")
        require(check["status"] == expected and bool(check.get("fix_id")) == fix,
                f"integration mismatch: {check}")
        require("adoption_required" not in check, "private UI field leaked into JSON")
        _, text = self.cli(config, "--check", structured=False)
        require(f"[{expected}] sway.integration" in text, "text/JSON integration status differs")
        return check

    def passed(self, scenario, observations):
        self.records.append({"scenario": scenario, "status": "passed", "observed": observations})
        print(f"PASS {scenario}: {observations}", flush=True)

    def backup(self, result, original):
        paths = result["doctor_fix"].get("backups", [])
        require(len(paths) == 1, f"expected one original backup, got {paths}")
        path = Path(paths[0])
        require(path.is_relative_to(self.root) and path.read_bytes() == original
                and path.stat().st_mode & 0o777 == 0o600, "original backup bytes/mode/root differ")

    def cli_scenarios(self):
        for default in (False, True):
            name = "cli-new-default" if default else "cli-new-none"
            config, owned = self.fixture(name)
            original = config.read_bytes()
            self.check(config, "unavailable")
            code, _ = self.cli(config, "--fix", "sway.integration", "--yes")
            require(code != 0 and not owned.exists() and config.read_bytes() == original,
                    "implicit adoption modified configuration")
            options = ["--fix", "sway.integration", "--adopt-standard"]
            if default:
                options += ["--shortcuts", "default"]
            code, preview = self.cli(config, *options)
            require(code == 0 and preview["preview"] and len(preview["doctor_plan"]["changes"]) == 2,
                    "adoption preview not exactly two files")
            require(not owned.exists() and config.read_bytes() == original, "preview wrote configuration")
            code, result = self.cli(config, *options, "--yes")
            require(code == 0 and owned.read_text() == snippet(self.binary, default), "wrong adopted profile")
            require("sway-session daemon" in result["doctor_fix"]["message"]
                    and "sway-session restore" in result["doctor_fix"]["message"],
                    "CLI omitted explicit first-start guidance")
            require(config.read_bytes().startswith(original), "adoption changed existing main content")
            self.backup(result, original)
            check = self.check(config, "ok")
            require("source files only" in check["hint"], "check omitted source-only limitation")
            preserved_main, preserved_owned = config.read_bytes(), owned.read_bytes()
            code, _ = self.cli(config, "--fix", "sway.integration", "--yes")
            require(code != 0 and config.read_bytes() == preserved_main and owned.read_bytes() == preserved_owned,
                    "unspecified profile did not preserve existing profile")
            code, switched = self.cli(config, "--fix", "sway.integration", "--shortcuts",
                                      "none" if default else "default", "--yes")
            require(code == 0 and config.read_bytes() == preserved_main
                    and owned.read_text() == snippet(self.binary, not default), "profile switch touched wrong content")
            self.backup(switched, preserved_owned)
            self.passed(name, "text/JSON, explicit adoption, read-only preview, 0600 original backup, preserve and switch")
        for name, profile, expected in (
                ("legacy", HEADER + f"exec --no-startup-id {self.binary} daemon\n", "warning"),
                ("foreign", "# foreign file\nexec /usr/bin/true\n", "unavailable"),
                ("manual-edit", snippet(self.binary) + "# local edit\n", "unavailable")):
            config, owned = self.fixture("cli-" + name, profile)
            original = config.read_bytes(), owned.read_bytes()
            self.check(config, expected, False)
            code, _ = self.cli(config, "--fix", "sway.integration", "--adopt-standard", "--shortcuts", "default", "--yes")
            require(code != 0 and original == (config.read_bytes(), owned.read_bytes()), "protected file was modified")
            self.passed("cli-" + name, "text/JSON and explicit repair refuse protected file without writes")
        for include in ("direct", "repeated", "indirect"):
            config, _ = self.fixture("cli-" + include, snippet(self.binary), include=include)
            check = self.check(config, "warning" if include == "indirect" else "ok")
            require(any(("not observed" if include == "indirect" else "present at") in line
                        for line in check["evidence"]), "direct/indirect evidence misreported")
            self.passed("cli-" + include, "literal direct include evidence matches selected main file; foreign dynamic commands ignored")
        config, _ = self.fixture("cli-without-dynamic", snippet(self.binary), dynamic=False)
        plain = self.check(config, "ok")
        config, _ = self.fixture("cli-with-dynamic", snippet(self.binary))
        dynamic = self.check(config, "ok")
        require((plain["status"], plain["detail"], plain["hint"]) == (dynamic["status"], dynamic["detail"], dynamic["hint"]),
                "foreign dynamic commands changed integration finding")
        self.passed("cli-dynamic-independent", "same status, detail and limitation with/without unrelated wallpaper/idle shell expressions")

    def startup_scenarios(self):
        for default in (False, True):
            directory = self.root / ("startup-default" if default else "startup-none")
            directory.mkdir(mode=0o700)
            (directory / "run").mkdir(mode=0o700)
            calls, inert = directory / "calls", directory / "sway-session"
            write(inert, f"#!/bin/sh\nprintf '%s\\n' \"$1\" >> '{calls}'\n", 0o700)
            config, owned = directory / "config", directory / "50-sway-session-doctor.conf"
            write(owned, snippet(inert, default))
            write(config, BASE + DYNAMIC + f'include "{owned}"\n')
            env = dict(self.env, XDG_RUNTIME_DIR=str(directory / "run"))
            env.pop("SWAYSOCK", None)
            sway = PrivateSway(directory, env, config)
            try:
                until(lambda: calls.exists() and len(calls.read_text().splitlines()) >= 2, "inert startup traces")
                sway.reload()
                sway.reload()
                time.sleep(0.3)
                require(sorted(calls.read_text().splitlines()) == ["daemon", "restore"], "startup/reload call counts differ")
                loaded = sway.ipc(9)["config"]
                require(str(owned) in loaded, "loaded main source omits direct include")
                self.passed(directory.name, "private Sway: daemon once, restore once; no additional calls after two reloads")
            finally:
                sway.close()

    def tui_scenarios(self):
        for width, height in ((80, 24), (48, 16)):
            for default in (False, True):
                name = f"tui-new-{'default' if default else 'none'}-{width}x{height}"
                config, owned = self.fixture(name)
                original = config.read_bytes()
                terminal = DoctorPTY(self.args(config), self.env, width, height, self.root / (name + ".pty"))
                try:
                    terminal.select_integration()
                    terminal.send("f")
                    terminal.wait("Adopt standard integration")
                    choices = terminal.scroll()
                    require(shown(choices, "does not remove them or verify effective load order"),
                            "adoption limitations not reachable by scrolling")
                    terminal.send("n")
                    require(not owned.exists() and config.read_bytes() == original,
                            "cancelled adoption changed files")
                    terminal.send("f")
                    terminal.send("a")
                    terminal.wait("Choose standard profile")
                    choices = terminal.scroll()
                    require(shown(choices, "competing bindings and load order manually"),
                            "profile limitations not reachable by scrolling")
                    terminal.send("1" if default else "\r")
                    terminal.wait("Repair: sway.integration")
                    terminal.scroll()
                    require(not owned.exists() and config.read_bytes() == original,
                            "TUI preview wrote configuration")
                    terminal.send("n")
                    require(not owned.exists() and config.read_bytes() == original,
                            "TUI preview cancellation wrote configuration")
                    terminal.send("f")
                    terminal.send("a")
                    terminal.send("1" if default else "\r")
                    terminal.wait("Repair: sway.integration")
                    terminal.send("y")
                    terminal.wait("Repair applied")
                    until(lambda: owned.exists(), "TUI-owned standard file")
                    require(owned.read_text() == snippet(self.binary, default), "TUI chose wrong new profile")
                    result_pages = terminal.scroll()
                    require(shown(result_pages, "next Sway session") and shown(result_pages, "Setup recheck completed."),
                            "full TUI result and recheck guidance not reachable")
                    require(shown(result_pages, "sway-session daemon") and shown(result_pages, "sway-session restore"),
                            "explicit first-start guidance not reachable in TUI result")
                    backups = list(config.parent.glob("*.sway-session.bak-*"))
                    require(len(backups) == 1 and backups[0].read_bytes() == original
                            and backups[0].stat().st_mode & 0o777 == 0o600,
                            "TUI private original backup missing")
                    terminal.send("\r")
                    preserved = config.read_bytes(), owned.read_bytes()
                    terminal.send("f")
                    terminal.wait("Choose standard profile")
                    terminal.send("\r")
                    terminal.wait("Repair preview unavailable")
                    preserved_pages = terminal.scroll()
                    require(shown(preserved_pages, "already has the requested profile")
                            and preserved == (config.read_bytes(), owned.read_bytes()),
                            "TUI unspecified profile did not preserve current profile")
                    terminal.send("\r")
                    terminal.send("f")
                    terminal.send("0" if default else "1")
                    terminal.wait("Repair: sway.integration")
                    terminal.send("y")
                    terminal.wait("Repair applied")
                    require(config.read_bytes() == preserved[0]
                            and owned.read_text() == snippet(self.binary, not default),
                            "TUI switch modified foreign/main directives")
                    switched_pages = terminal.scroll()
                    require(shown(switched_pages, "Reload Sway manually") and shown(switched_pages, "reload does not run them"),
                            "profile-only switch guidance not reachable")
                    self.passed(name, "actual NO_COLOR PTY: choices, complete scrolling, preview/cancel/apply, private backup, preserve, switch, recheck")
                finally:
                    terminal.close()
            for kind, profile, message in (
                    ("legacy", HEADER + f"exec --no-startup-id {self.binary} daemon\n", "legacy partial"),
                    ("foreign", "# foreign file\nexec /usr/bin/true\n", "protected"),
                    ("manual-edit", snippet(self.binary) + "# manual edit\n", "protected")):
                name = f"tui-{kind}-{width}x{height}"
                config, owned = self.fixture(name, profile)
                original = config.read_bytes(), owned.read_bytes()
                terminal = DoctorPTY(self.args(config), self.env, width, height, self.root / (name + ".pty"))
                try:
                    terminal.select_integration()
                    pages = terminal.scroll()
                    require(shown(pages, message) and shown(pages, "manually"), "manual migration guidance not reachable")
                    terminal.send("f")
                    require("Choose standard profile" not in terminal.screen.text()
                            and "Adopt standard integration" not in terminal.screen.text()
                            and original == (config.read_bytes(), owned.read_bytes()),
                            "TUI offered repair or altered protected configuration")
                    self.passed(name, "actual PTY: manual migration/protected-file guidance reachable; no repair or write")
                finally:
                    terminal.close()
            name = f"tui-stale-{width}x{height}"
            config, owned = self.fixture(name)
            original = config.read_bytes()
            terminal = DoctorPTY(self.args(config), self.env, width, height, self.root / (name + ".pty"))
            try:
                terminal.select_integration()
                terminal.send("f")
                terminal.send("a")
                terminal.send("\r")
                terminal.wait("Repair: sway.integration")
                changed = original + b"# concurrent editor retained\n"
                config.write_bytes(changed)
                terminal.send("y")
                terminal.wait("Repair failed")
                terminal.scroll()
                require(not owned.exists() and config.read_bytes() == changed, "stale TUI plan replaced concurrent content")
                self.passed(name, "actual PTY preview-to-apply editor change: stale plan rejected, editor save preserved, standard file absent")
            finally:
                terminal.close()

    def close(self):
        self.sway.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--keep", action="store_true", help="retain disposable /tmp diagnostics")
    parser.add_argument("--skip-tui", action="store_true", help="mark actual PTY scenarios not run")
    options = parser.parse_args()
    binary = options.binary.resolve(strict=True)
    require(binary.is_file() and os.access(binary, os.X_OK), "candidate is not executable")
    require(binary.name == "sway-session", "candidate basename must be sway-session for safe repair")
    require(Path("/usr/bin/sway").is_file(), "Sway is required")
    root = Path(tempfile.mkdtemp(prefix="lab326-", dir="/tmp"))
    acceptance = None
    try:
        acceptance = Acceptance(binary, root)
        identity = subprocess.run([str(binary), "version", "--json"], env=acceptance.env,
                                  capture_output=True, check=True, timeout=5)
        evidence = {"candidate": json.loads(identity.stdout),
                    "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                    "sway": subprocess.check_output(["/usr/bin/sway", "--version"], env=acceptance.env).decode().strip()}
        acceptance.cli_scenarios()
        acceptance.startup_scenarios()
        if options.skip_tui:
            acceptance.records.append({"scenario": "real-TUI-PTY", "status": "not run", "observed": "--skip-tui selected"})
        else:
            acceptance.tui_scenarios()
        require(not any((root / "state").iterdir()), "Doctor created session state")
        require(not (root / "run" / "sway-session").exists(), "Doctor initialized its daemon/runtime state")
        require(acceptance.reload_calls.read_text().splitlines() == ["load"], "Doctor reloaded private Sway")
        acceptance.passed("doctor-no-runtime-effects", "private exec_always observer saw startup only; no compositor reload or sway-session runtime/session state")
        evidence["scenarios"] = acceptance.records
        (root / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        print(json.dumps(evidence, indent=2))
    finally:
        if acceptance is not None:
            acceptance.close()
        if options.keep:
            print(f"Private diagnostics retained: {root}")
        else:
            shutil.rmtree(root)


if __name__ == "__main__":
    main()
