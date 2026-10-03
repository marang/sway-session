#!/usr/bin/env python3
"""Root control harness for the disposable LAB-177 Arch KVM guest.

The parent uploads run-id, sway-session, observe and herdr here before install.
Only baseline changes compositor/session state; observe performs reads only.
"""
import argparse
import hashlib
import json
import os
import pwd
import re
import shlex
import shutil
import stat
import subprocess
import sys
import time
import uuid
from pathlib import Path

from transport import run_command

ROOT = Path('/opt/sway-session-test')
USER = 'ss-test'
HOME = Path('/home/ss-test')
CANDIDATE = Path('/usr/local/bin/sway-session')
LIMIT = 2 * 1024 * 1024
HARNESS = ROOT / 'command-harness.json'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def command(argv, *, timeout=10):
    return run_command(argv, timeout=timeout, limit=LIMIT, env={
        'PATH': '/usr/local/bin:/usr/bin', 'LC_ALL': 'C',
        'SYSTEMD_COLORS': '0', 'SYSTEMD_PAGER': 'cat', 'COLUMNS': '4096',
    }).decode('utf-8')


def private_artifact(path, *, executable=False):
    info = path.lstat()
    require(path.resolve() == path and stat.S_ISREG(info.st_mode) and info.st_uid == 0
            and info.st_nlink == 1 and not info.st_mode & 0o022
            and 0 < info.st_size <= 256 * 1024 * 1024,
            f'expected trusted bounded root-owned artifact: {path}')
    if executable:
        require(os.access(path, os.X_OK), f'artifact is not executable: {path}')


def guard():
    require(os.geteuid() == 0, 'guest harness requires root SSH control')
    require(command(['systemd-detect-virt', '--vm']).strip() == 'kvm',
            'refusing fixture operation outside a KVM VM')
    info = ROOT.lstat()
    require(ROOT.resolve() == ROOT and stat.S_ISDIR(info.st_mode)
            and info.st_uid == 0 and not info.st_mode & 0o022,
            'fixture artifact directory must be trusted and root-owned')
    marker = ROOT / 'run-id'
    private_artifact(marker)
    run_id = marker.read_text().strip()
    parsed = uuid.UUID(run_id)
    require(str(parsed) == run_id and parsed.version == 4,
            'run-id must be the parent-provided canonical random UUID v4')
    account = pwd.getpwnam(USER)
    require(account.pw_uid > 0 and account.pw_dir == str(HOME)
            and account.pw_shell in ('/bin/bash', '/usr/bin/bash'),
            'expected disposable ss-test account with /home/ss-test and Bash')
    require(HOME.resolve() == HOME and HOME.stat().st_uid == account.pw_uid,
            'disposable guest home is not owned by ss-test')
    return run_id, account


def doctor_evidence(report):
    rows = report.get('doctor', {}).get('checks', [])
    selected = []
    pids = []
    for check_id in ['daemon.lock', 'daemon.binary']:
        matches = [row for row in rows if row.get('id') == check_id]
        require(len(matches) == 1 and matches[0].get('status') == 'ok',
                f'doctor {check_id} must prove a healthy current daemon')
        row = matches[0]
        evidence = row.get('evidence', [])
        found = [int(value[4:]) for value in evidence if re.fullmatch(r'pid=[1-9][0-9]*', value)]
        require(len(found) == 1, f'doctor {check_id} omitted an unambiguous daemon PID')
        pids.append(found[0])
        if check_id == 'daemon.binary':
            require('path=' + str(CANDIDATE) in evidence
                    and ('inode match' in evidence or 'digest match' in evidence),
                    'doctor did not verify the installed candidate artifact')
        selected.append({'id': check_id, 'status': row['status']})
    require(pids[0] == pids[1], 'daemon identity changed between doctor checks')
    return pids[0], selected


def inhibitor_evidence(output, pid, uid):
    """Read systemd's eight-column inhibitor table; never expose WHY text."""
    if output.lstrip().startswith('['):
        rows = json.loads(output)
        require(isinstance(rows, list), 'inhibitor JSON must contain a table')
    else:
        rows = []
        for line in output.splitlines():
            fields = line.split()
            if len(fields) >= 8 and fields[0] == 'sway-session' and fields[1].isdigit() and fields[3].isdigit():
                rows.append({'who': fields[0], 'uid': int(fields[1]), 'pid': int(fields[3]),
                             'what': fields[5], 'mode': fields[-1]})
    matches = [row for row in rows if row.get('who') == 'sway-session'
               and row.get('uid') == uid and row.get('pid') == pid
               and row.get('mode') == 'delay'
               and set(row.get('what', '').split(':')) == {'shutdown', 'sleep'}]
    require(len(matches) == 1, 'exact daemon PID/UID must hold one shutdown:sleep delay inhibitor')
    return {'who': 'sway-session', 'uid': uid, 'pid': pid, 'what': 'shutdown:sleep', 'mode': 'delay'}


def read_json(text):
    require(len(text.encode()) <= LIMIT, 'JSON exceeds fixture budget')
    return json.loads(text)


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def artifact_identity():
    private_artifact(CANDIDATE, executable=True)
    build = read_json(command([CANDIDATE, '--json', 'version']))['build']
    return {'sha256': digest(CANDIDATE),
            'build': {key: build[key] for key in ['product_version', 'commit', 'modified']}}


def write_harness(value):
    if HARNESS.exists():
        private_artifact(HARNESS)
    with HARNESS.open('w', encoding='utf-8') as dest:
        json.dump(value, dest)
        dest.write('\n')
    HARNESS.chmod(0o600)


def load_harness(run_id):
    private_artifact(HARNESS)
    value = read_json(HARNESS.read_text())
    require(value['run_id'] == run_id, 'installed harness belongs to a different run-id')
    return value


def install(run_id, account):
    require(not HARNESS.exists(), 'fixture is already installed; provision a fresh VM for a new run')
    for name in ['guest.py', 'transport.py', 'sway-session', 'observe', 'herdr']:
        private_artifact(ROOT / name)
    for relative in ['project', '.config', '.local/state']:
        require(not (HOME / relative).exists(), f'fresh disposable account required: {relative} already exists')
    for program in ['sway', 'swaymsg', 'alacritty', 'dbus-run-session', 'agetty', 'runuser', 'busctl', 'systemd-inhibit', 'pkcheck']:
        require(shutil.which(program, path='/usr/bin'), f'guest package tool is missing: {program}')
    for name in ['sway-session', 'herdr']:
        target = Path('/usr/local/bin') / name
        require(not target.is_symlink(), f'unsafe executable destination: {target}')
        shutil.copyfile(ROOT / name, target)
        target.chmod(0o755)
    (ROOT / 'observe').chmod(0o755)
    HOME.chmod(0o700)
    for relative in ['project', '.config/sway', '.config/sway-session', '.config/herdr',
                     '.local/state', '.local/share', '.cache']:
        path = HOME / relative
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
        # mkdir's intermediate directories also belong to this fixture user.
        for parent in [path, *path.parents]:
            if parent == HOME:
                break
            parent.chmod(0o700)
            os.chown(parent, account.pw_uid, account.pw_gid)

    def user_file(relative, content):
        path = HOME / relative
        require(not path.is_symlink(), f'unsafe fixture config: {relative}')
        path.write_text(content)
        path.chmod(0o600)
        os.chown(path, account.pw_uid, account.pw_gid)

    user_file('.config/sway-session/config.toml',
              'version = 2\n[terminal]\nadapter = "alacritty"\nsession_manager = "herdr"\n')
    user_file('.config/herdr/config.toml', '[experimental]\npane_history = true\n')
    user_file('.config/sway/config', '''xwayland disable
output HEADLESS-1 resolution 1280x800
font pango:DejaVu Sans Mono 10
workspace 98
exec --no-startup-id /usr/local/bin/sway-session daemon
exec --no-startup-id /usr/local/bin/sway-session restore
bindsym Mod4+Return exec --no-startup-id /usr/local/bin/sway-session terminal --new
bindsym Mod4+Shift+Return exec --no-startup-id /usr/local/bin/sway-session terminal --ephemeral
''')
    user_file('.bash_profile', '''# Disposable KVM fixture: a real getty/PAM/logind login owns Sway.
umask 077
export PATH=/usr/local/bin:/usr/bin SHELL=/bin/bash
export XDG_CONFIG_HOME="$HOME/.config" XDG_STATE_HOME="$HOME/.local/state"
export XDG_DATA_HOME="$HOME/.local/share" XDG_CACHE_HOME="$HOME/.cache"
export WLR_BACKENDS=headless WLR_HEADLESS_OUTPUTS=1 WLR_RENDERER=pixman
export WLR_LIBINPUT_NO_DEVICES=1 LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe
if [ "$(tty)" = /dev/tty1 ] && [ -z "${SWAYSOCK:-}" ]; then
    exec /usr/bin/dbus-run-session /usr/bin/sway --config "$HOME/.config/sway/config" >> "$XDG_STATE_HOME/sway-fixture.log" 2>&1
fi
''')
    directory = Path('/etc/systemd/system/getty@tty1.service.d')
    directory.mkdir(parents=True, exist_ok=True)
    (directory / 'ss-test.conf').write_text('''[Service]
ExecStart=
ExecStart=/usr/bin/agetty --autologin ss-test --noclear %I $TERM
''')
    write_harness({'version': 1, 'run_id': run_id, 'artifact': artifact_identity(),
                   'expected_ids': [], 'baseline_started': False,
                   'startup': 'getty@tty1/PAM/logind -> Bash -> headless Sway -> candidate daemon + one-shot restore'})
    command(['systemctl', 'daemon-reload'])
    command(['systemctl', 'enable', 'getty@tty1.service'])
    # This first install applies the getty fixture. Reboots use enabled startup;
    # baseline and observe never start or restart a service or daemon.
    command(['systemctl', 'restart', 'getty@tty1.service'])
    return {'installed': True, 'run_id': run_id}


def guest_socket(account):
    runtime = Path('/run/user') / str(account.pw_uid)
    info = runtime.lstat()
    require(runtime.resolve() == runtime and stat.S_ISDIR(info.st_mode)
            and info.st_uid == account.pw_uid and not info.st_mode & 0o077,
            'PAM/logind has not created the private ss-test runtime directory')
    sockets = sorted(runtime.glob(f'sway-ipc.{account.pw_uid}.*.sock'))
    require(len(sockets) == 1, f'expected exactly one ss-test Sway IPC socket; found {len(sockets)}')
    info = sockets[0].lstat()
    require(stat.S_ISSOCK(info.st_mode) and info.st_uid == account.pw_uid,
            'fixture Sway endpoint must be a socket owned by ss-test')
    return sockets[0]


def user_command(account, argv, socket):
    runtime = socket.parent
    displays = [p for p in runtime.glob('wayland-*') if stat.S_ISSOCK(p.lstat().st_mode)]
    require(len(displays) == 1, 'expected exactly one fixture Wayland display')
    environment = {
        'HOME': str(HOME), 'USER': USER, 'LOGNAME': USER, 'SHELL': '/bin/bash',
        'PATH': '/usr/local/bin:/usr/bin', 'LANG': 'C.UTF-8', 'LC_ALL': 'C.UTF-8',
        'XDG_RUNTIME_DIR': str(runtime), 'SWAYSOCK': str(socket),
        'WAYLAND_DISPLAY': displays[0].name, 'TERM': 'xterm-256color',
        'XDG_CONFIG_HOME': str(HOME / '.config'), 'XDG_STATE_HOME': str(HOME / '.local/state'),
        'XDG_DATA_HOME': str(HOME / '.local/share'), 'XDG_CACHE_HOME': str(HOME / '.cache'),
        'LIBGL_ALWAYS_SOFTWARE': '1', 'GALLIUM_DRIVER': 'llvmpipe',
    }
    return command(['runuser', '-u', USER, '--', '/usr/bin/env', '-i',
                    *[f'{key}={value}' for key, value in environment.items()], *argv], timeout=20)


def login_evidence(pid):
    destination = 'org.freedesktop.login1'
    manager = 'org.freedesktop.login1.Manager'
    reply = shlex.split(command(['busctl', 'call', destination, '/org/freedesktop/login1',
                                manager, 'GetSessionByPID', 'u', str(pid)]))
    require(len(reply) == 2 and reply[0] == 'o'
            and re.fullmatch(r'/org/freedesktop/login1/session/[A-Za-z0-9_]+', reply[1]),
            'daemon does not belong to a verifiable logind session')
    values = {}
    for property_name in ['TTY', 'Service', 'State']:
        value = shlex.split(command(['busctl', 'get-property', destination, reply[1],
                                     'org.freedesktop.login1.Session', property_name]))
        require(len(value) == 2 and value[0] == 's', 'invalid logind session property')
        values[property_name.lower()] = value[1]
    require(values == {'tty': 'tty1', 'service': 'login', 'state': 'active'},
            'candidate daemon must belong to the active tty1 PAM/login session')
    require(command(['systemctl', 'is-active', 'getty@tty1.service']).strip() == 'active',
            'automatic tty1 startup service is not active')
    values['method'] = 'getty@tty1/PAM/logind -> Bash -> headless Sway -> candidate daemon + one-shot restore'
    values['sway_exec'] = ['sway-session daemon', 'sway-session restore']
    return values


def health(account, socket):
    report = read_json(user_command(account, [CANDIDATE, '--json', 'doctor', '--check',
                           '--socket', socket, '--sway-config', HOME / '.config/sway/config'], socket))
    pid, checks = doctor_evidence(report)
    try:
        output = command(['systemd-inhibit', '--list', '--no-pager', '--json=short'])
    except RuntimeError as error:
        # Older systemd can lack JSON support; bus/permission failures still fail.
        require('unrecognized option' in str(error) and 'json' in str(error), str(error))
        output = command(['systemd-inhibit', '--list', '--no-pager', '--no-legend'])
    inhibitor = inhibitor_evidence(output, pid, account.pw_uid)
    return {'daemon_binary_current': True, 'delay_inhibitor': True, 'daemon_pid': pid,
            'doctor': checks, 'inhibitor': inhibitor, 'startup': login_evidence(pid)}


def versions():
    packages = ['sway', 'alacritty', 'mesa', 'ttf-dejavu', 'python', 'jq', 'sqlite', 'dbus', 'openssh', 'polkit']
    rows = command(['pacman', '-Q', *packages]).splitlines()
    values = dict(row.split(maxsplit=1) for row in rows)
    require(set(values) == set(packages), 'guest package version evidence is incomplete')
    return {'kernel': Path('/proc/sys/kernel/osrelease').read_text().strip(), 'packages': values,
            'systemd': command(['systemctl', '--version']).splitlines()[0],
            'herdr': command(['/usr/local/bin/herdr', '--version']).strip(),
            'observer_sha256': digest(ROOT / 'observe')}


def observe(run_id, account):
    harness = load_harness(run_id)
    ids = harness['expected_ids']
    require(len(ids) == 2 and len(set(ids)) == 2
            and all(str(uuid.UUID(value)) == value for value in ids),
            'baseline has not recorded two exact fixture UUIDs')
    private_artifact(ROOT / 'observe', executable=True)
    socket = guest_socket(account)
    observation = read_json(user_command(account, [ROOT / 'observe',
                            '--state-root', HOME / '.local/state/sway-session',
                            '--socket', socket, '--expected', ','.join(ids)], socket))
    observation.update(health(account, socket))
    artifact = artifact_identity()
    require(artifact == harness['artifact'], 'installed candidate identity changed since install')
    observation.update({'run_id': run_id,
                        'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                        'artifact': artifact, 'versions': versions()})
    return observation


def until_ready(operation, deadline, *, stable=False):
    last = 'fixture not ready'
    previous, count = None, 0
    while time.monotonic() < deadline:
        try:
            value = operation()
            count = count + 1 if value == previous else 1
            previous = value
            if not stable or count >= 3:
                return value
        except (RuntimeError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
            last, previous, count = str(error), None, 0
        time.sleep(1)
    raise RuntimeError(f'baseline readiness deadline exceeded: {last}')


def sway_command(account, socket, text):
    replies = read_json(user_command(account, ['swaymsg', '-r', '-s', socket, text], socket))
    require(isinstance(replies, list) and replies and all(row.get('success') is True for row in replies),
            'Sway rejected fixture workspace/layout setup')


def mapped(account, socket, ids):
    tree = read_json(user_command(account, ['swaymsg', '-r', '-s', socket, '-t', 'get_tree'], socket))
    leaves, pending = [], [tree]
    visited = 0
    while pending:
        node = pending.pop()
        visited += 1
        require(visited <= 4096, 'baseline Sway tree exceeds fixture node budget')
        if (node.get('app_id') or '').startswith('sway-session.'):
            leaves.append(node['app_id'][len('sway-session.'):])
        pending.extend(node.get('nodes', []))
        pending.extend(node.get('floating_nodes', []))
    require(sorted(leaves) == sorted(ids), 'waiting for exact fixture terminal windows to map')
    return True


def terminal_id(reply, existing):
    terminal = reply.get('terminal', {})
    require(reply.get('command') == 'terminal' and 'created' in terminal.get('actions', []),
            'real terminal CLI did not create a fresh context')
    value = terminal['context_id']
    require(str(uuid.UUID(value)) == value and value not in existing
            and [c['id'] for c in reply['contexts']] == [value],
            'terminal CLI did not return one fresh exact context UUID')
    return value


def baseline(run_id, account):
    deadline = time.monotonic() + 160
    harness = load_harness(run_id)
    require(not harness['baseline_started'], 'baseline already started; provision a fresh VM instead of creating duplicates')

    def startup_ready():
        socket = guest_socket(account)
        health(account, socket)
        return socket

    socket = until_ready(startup_ready, deadline)
    harness['baseline_started'] = True
    write_harness(harness)
    sway_command(account, socket, 'workspace 98')
    ids = []
    for _ in range(2):
        reply = read_json(user_command(account, [CANDIDATE, '--json', 'terminal', '--new',
                           '--cwd', HOME / 'project'], socket))
        ids.append(terminal_id(reply, ids))
        until_ready(lambda: mapped(account, socket, ids), deadline)
    sway_command(account, socket, 'workspace 98; layout tabbed')
    harness['expected_ids'] = ids
    write_harness(harness)
    # Canonical store/capture observations must agree repeatedly. No database
    # seeding, manual capture, restore command or daemon restart is involved.
    return until_ready(lambda: observe(run_id, account), deadline, stable=True)


def main(arguments=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=['install', 'baseline', 'observe'])
    args = parser.parse_args(arguments)
    run_id, account = guard()
    result = {'install': install, 'baseline': baseline, 'observe': observe}[args.operation](run_id, account)
    encoded = json.dumps(result, separators=(',', ':'))
    require(len(encoded) <= LIMIT, 'guest observation exceeds JSON budget')
    print(encoded)


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print(f'VM guest: {error}', file=sys.stderr)
        sys.exit(1)
