#!/usr/bin/env python3
"""Real desktop acceptance control, exclusively inside an owned KVM guest.

All application content, screenshots and diagnostic streams remain guest-local.
Only canonical structural observations and explicit typed facts may leave it.
"""
import argparse
import json
import os
import re
import shutil
import stat
import subprocess
import tarfile
import time
from pathlib import Path

import guest
import desktop_trace

CANDIDATE = Path('/usr/bin/sway-session')
DESKTOP_ID = 'lab253-chrome.desktop'
ENTRY = guest.HOME / '.local/share/applications' / DESKTOP_ID
PROFILE = guest.HOME / '.config/lab253-chrome'
IDENTITY_KEYS = ['protocol', 'app_id', 'x11_class', 'x11_instance', 'sandbox_app_id',
                 'sandbox_engine', 'sandbox_instance_present']


def user(account, argv, socket, *, timeout=20):
    return guest.user_command(account, argv, socket,
                              extra_env=guest.desktop_environment(account, socket), timeout=timeout)


def cli(account, socket, *args):
    return guest.read_json(user(account, [CANDIDATE, '--json', *args], socket))


def cli_attempt(account, socket, *args):
    """Retain failed CLI JSON privately; the transport normally discards it."""
    output = guest.HOME / '.cache/lab253/cli-result.json'
    errors = output.with_name('cli-error.json')
    write_user(account, output, '{}\n')
    write_user(account, errors, '{}\n')
    status = user(account, ['/bin/sh', '-c',
                           'out="$1"; err="$2"; shift 2; "$@" > "$out" 2> "$err"; printf "%s" "$?"',
                           'lab253-cli', output, errors, CANDIDATE, '--json', *args], socket)
    guest.require(re.fullmatch(r'[0-9]{1,3}', status) is not None, 'invalid_cli_exit_status')
    if int(status) != 0:
        output = errors
    info = output.lstat()
    guest.require(stat.S_ISREG(info.st_mode) and info.st_size <= guest.LIMIT and not info.st_mode & 0o077,
                  'invalid_private_cli_reply')
    return int(status), guest.read_json(output.read_text())


def probe(account, socket):
    value = guest.read_json(user(account, [guest.ROOT / 'observe', '--scenario', 'desktop', '--probe',
                                        '--state-root', guest.HOME / '.local/state/sway-session',
                                        '--socket', socket], socket))
    guest.require(value['scenario'] == 'desktop_probe', 'desktop_probe_required')
    return value


def wait_for(operation, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = operation()
        if value:
            return value
        time.sleep(.1)
    raise RuntimeError('desktop_condition_timeout')


def write_user(account, path, content, mode=0o600):
    guest.require(path.is_relative_to(guest.HOME) and path.resolve() == path, 'unsafe_fixture_user_path')
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    for directory in [path.parent, *path.parent.parents]:
        if directory == guest.HOME:
            break
        os.chown(directory, account.pw_uid, account.pw_gid)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, mode)
    with os.fdopen(descriptor, 'w') as output:
        output.write(content)
    os.chmod(path, mode)
    os.chown(path, account.pw_uid, account.pw_gid)


def chrome_entry(protocol='wayland', *, changed=False):
    guest.require(protocol in ['wayland', 'x11'], 'invalid_chrome_protocol')
    extra = ' --disable-features=Translate' if changed else ''
    return ('[Desktop Entry]\nType=Application\nName=LAB253 Disposable Chrome\n'
            f'Exec=/opt/google/chrome/chrome --ozone-platform={protocol} --user-data-dir={PROFILE}'
            ' --no-first-run --no-default-browser-check --password-store=basic'
            ' --disable-background-networking --disable-component-update --disable-sync'
            f' --disable-extensions --disable-gpu{extra} about:blank\nStartupWMClass=google-chrome\n')


def launch_entry(account, socket, entry=ENTRY):
    # Sway supplies its own private display/session environment to bootstrap.
    # Actual restore remains the production candidate GIO launcher.
    guest.require(entry == ENTRY, 'unexpected_bootstrap_entry')
    guest.sway_command(account, socket, 'exec --no-startup-id /usr/bin/gio launch ' + str(entry))


def approval_material():
    root = guest.HOME / '.local/state/sway-session/desktop-approvals'
    if not root.exists():
        return {}
    result = {}
    for path in root.iterdir():
        info = path.lstat()
        guest.require(stat.S_ISREG(info.st_mode) and not info.st_mode & 0o077 and info.st_size <= 262144,
                      'unsafe_approval_material')
        result[path.name] = guest.digest(path)
    guest.require(len(result) <= 8, 'approval_material_budget')
    return result


def swaynag_pids(account):
    result = []
    for path in Path('/proc').iterdir():
        if not path.name.isdigit():
            continue
        try:
            if path.stat().st_uid == account.pw_uid and (path / 'exe').resolve() == Path('/usr/bin/swaynag'):
                result.append(int(path.name))
        except (FileNotFoundError, PermissionError):
            continue
    guest.require(len(result) <= 1, 'multiple_approval_surfaces')
    return result


def screenshot(account, socket, name):
    guest.require(re.fullmatch(r'[a-z0-9-]+', name) is not None, 'invalid_screenshot_name')
    # The file is private to the disposable user. The parent may inspect a
    # private /tmp copy, but it never enters acceptance JSON or CI artifacts.
    directory = guest.HOME / '.cache/lab253'
    directory.mkdir(mode=0o700, exist_ok=True)
    os.chown(directory, account.pw_uid, account.pw_gid)
    path = directory / (name + '.png')
    user(account, ['grim', '-o', 'HEADLESS-1', path], socket)
    info = path.lstat()
    guest.require(info.st_uid == account.pw_uid and stat.S_ISREG(info.st_mode)
                  and 0 < info.st_size <= 8 * 1024 * 1024, 'invalid_private_screenshot')
    path.chmod(0o600)
    return path


def click(account, socket, x, y):
    guest.require(type(x) is int and type(y) is int and 0 <= x < 1280 and 0 <= y < 720,
                  'invalid_fixture_pointer_coordinate')
    for action in [f'set {x} {y}', 'press button1', 'release button1']:
        guest.sway_command(account, socket, 'seat seat0 cursor ' + action)


def pointer_ready(run_id, account, socket):
    def ready():
        rows = guest.read_json(user(account, ['swaymsg', '-r', '-s', socket, '-t', 'get_seats'], socket))
        return len(rows) == 1 and rows[0]['name'] == 'seat0' and rows[0]['capabilities'] & 1 != 0
    if ready():
        return
    write_user(account, guest.HOME / '.cache/lab253/pointer-owner.json', json.dumps({
        'run_id': run_id, 'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
        'socket': str(socket)}))
    helper = guest.ROOT / 'desktop_pointer.py'
    guest.private_artifact(helper)
    helper.chmod(0o644)
    guest.sway_command(account, socket, 'exec --no-startup-id /usr/bin/python3 ' + str(helper) + ' --run-id ' + run_id)
    wait_for(ready, seconds=10)


def open_approval(account, socket, command):
    guest.require(not swaynag_pids(account), 'approval_surface_already_open')
    cli(account, socket, *command)
    wait_for(lambda: swaynag_pids(account))
    # get_outputs exposes actual layer surfaces only on some Sway releases;
    # visual inspection of the private screenshot establishes mapped UI.
    time.sleep(.25)
    return screenshot(account, socket, 'approval-pending')


def register_action(run_id, account, socket, role, *, approve, x, y, inspected=False):
    guest.require(role in ['chrome', 'slack'] and inspected, 'visual_inspection_required')
    before = probe(account, socket)
    material = approval_material()
    trace = desktop_trace.snapshot(run_id)
    guest.require(swaynag_pids(account), 'approval_surface_missing')
    click(account, socket, x, y)
    expected = len(before['groups']) + int(approve)
    after = wait_for(lambda: (value if len((value := probe(account, socket))['groups']) == expected else None))
    if approve and swaynag_pids(account):
        # The production presenter uses swaynag -b, whose documented behavior
        # keeps the bar open after invoking its action. Close only after the
        # canonical registry proves the actual callback committed.
        click(account, socket, 1268, 16)
    wait_for(lambda: not swaynag_pids(account))
    trace_after = desktop_trace.snapshot(run_id)
    callback = trace_after['callbacks'] > trace['callbacks']
    guest.require(callback == approve, 'approval_callback_attribution_failed')
    saved = approval_material()
    context_id = repeat_id = ''
    if approve:
        created = set(group['context_id'] for group in after['groups']) - set(group['context_id'] for group in before['groups'])
        guest.require(len(created) == 1, 'approval_context_delta')
        context_id = created.pop()
        args = ['app', 'register-focused']
        if role == 'chrome':
            args += ['--desktop-id', DESKTOP_ID]
        reply = cli(account, socket, *args)
        contexts = reply.get('contexts', [])
        guest.require(len(contexts) == 1 and contexts[0]['id'] == context_id, 'approval_repeat_not_idempotent')
        repeat_id = contexts[0]['id']
        guest.require(not swaynag_pids(account), 'approval_repeat_prompted')
    else:
        guest.require(saved == material, 'cancel_changed_approval_material')
    return {'surface': 'swaynag', 'surface_mapped': True, 'visually_inspected': True,
            'input': 'sway-ipc-pointer', 'callback_invoked': callback,
            'registration_before': len(before['groups']), 'registration_after': len(after['groups']),
            'approval_material_before': len(material), 'approval_material_after': len(saved),
            'context_id': context_id, 'repeat_context_id': repeat_id}


def versions(account, socket):
    result = guest.versions()
    version_path = guest.ROOT / 'desktop-chrome-version'
    guest.private_artifact(version_path)
    chrome_version = version_path.read_text().strip()
    guest.require(re.fullmatch(r'[0-9]+(?:\.[0-9]+){3}', chrome_version) is not None, 'invalid_chrome_version')
    result.update({'chrome': chrome_version,
                   'slack': user(account, ['flatpak', 'list', '--user', '--app', '--columns=application,version'], socket).strip(),
                   'flatpak': guest.command(['flatpak', '--version']).strip().removeprefix('Flatpak '),
                   'slack_ref': user(account, ['flatpak', 'info', '--user', '--show-ref', 'com.slack.Slack'], socket).strip(),
                   'slack_commit': user(account, ['flatpak', 'info', '--user', '--show-commit', 'com.slack.Slack'], socket).strip()})
    rows = [line.split('\t') for line in result['slack'].splitlines()]
    matches = [row[1] for row in rows if len(row) == 2 and row[0] == 'com.slack.Slack']
    guest.require(len(matches) == 1, 'slack_version_missing')
    result['slack'] = matches[0]
    result['herdr'] = result['herdr'].removeprefix('herdr ')
    extras = ['flatpak', 'bubblewrap', 'xorg-xwayland', 'grim', 'wtype', 'noto-fonts',
              'xdg-desktop-portal', 'xdg-desktop-portal-wlr']
    packages = dict(row.split(maxsplit=1) for row in guest.command(['pacman', '-Q', *extras]).splitlines())
    guest.require(set(packages) == set(extras), 'desktop_package_versions_missing')
    result['packages'].update(packages)
    return result


def install_apps(run_id, account, chrome_sha256):
    guest.require(isinstance(chrome_sha256, str) and re.fullmatch(r'[a-f0-9]{64}', chrome_sha256) is not None,
                  'invalid_chrome_digest')
    package = guest.ROOT / 'google-chrome.deb'
    guest.private_artifact(package)
    guest.require(guest.digest(package) == chrome_sha256, 'chrome_package_digest_mismatch')
    guest.require(not Path('/opt/google/chrome').exists(), 'chrome_already_installed')
    # This package operation is guarded by main/guest.guard before any mutation.
    guest.command(['pacman', '-S', '--noconfirm', '--needed', 'flatpak', 'xorg-xwayland', 'grim',
                   'wtype', 'noto-fonts', 'xdg-desktop-portal', 'xdg-desktop-portal-wlr',
                   'alsa-lib', 'at-spi2-core', 'cairo', 'libcups', 'expat', 'fontconfig', 'glib2',
                   'gtk3', 'libdrm', 'libx11', 'libxcb', 'libxcomposite', 'libxdamage', 'libxext',
                   'libxfixes', 'libxkbcommon', 'libxrandr', 'nss', 'pango', 'vulkan-icd-loader',
                   'xdg-utils'], timeout=600)
    staging = guest.ROOT / 'chrome-vendor-package'
    staging.mkdir(mode=0o700)
    guest.command(['bsdtar', '-xf', package, '-C', staging], timeout=60)
    payloads = list(staging.glob('data.tar.*'))
    guest.require(len(payloads) == 1, 'chrome_vendor_payload_missing')
    unpacked = staging / 'payload'
    unpacked.mkdir(mode=0o700)
    with tarfile.open(payloads[0]) as archive:
        members = []
        for member in archive:
            parts = Path(member.name).parts
            guest.require(not Path(member.name).is_absolute() and '..' not in parts,
                          'unsafe_vendor_archive_path')
            normalized = member.name.removeprefix('./').rstrip('/')
            if normalized == 'opt/google/chrome' or normalized.startswith('opt/google/chrome/') or normalized == 'usr/share/applications/google-chrome.desktop':
                members.append(member)
        guest.require(0 < len(members) <= 4096 and sum(item.size for item in members) <= 1024**3,
                      'chrome_vendor_payload_budget')
        archive.extractall(unpacked, members=members, filter='data')
    Path('/opt/google').mkdir(exist_ok=True)
    shutil.copytree(unpacked / 'opt/google/chrome', '/opt/google/chrome', symlinks=True)
    sandbox = Path('/opt/google/chrome/chrome-sandbox')
    guest.require(sandbox.is_file() and not sandbox.is_symlink(), 'chrome_vendor_sandbox_missing')
    sandbox.chmod(0o4755)
    for filename in ['google-chrome-stable', 'google-chrome']:
        target = Path('/usr/bin') / filename
        guest.require(not target.exists() and not target.is_symlink(), 'chrome_binary_destination_exists')
        target.symlink_to('/opt/google/chrome/google-chrome')
    shutil.copyfile(unpacked / 'usr/share/applications/google-chrome.desktop', '/usr/share/applications/google-chrome.desktop')
    Path('/usr/share/applications/google-chrome.desktop').chmod(0o644)
    shutil.rmtree(staging)
    write_user(account, ENTRY, chrome_entry())
    socket = wait_for(lambda: guest.guest_socket(account))
    user(account, ['flatpak', 'remote-add', '--user', '--if-not-exists', 'flathub',
                   'https://flathub.org/repo/flathub.flatpakrepo'], socket, timeout=120)
    user(account, ['flatpak', 'install', '--user', '--noninteractive', '--assumeyes',
                   'flathub', 'com.slack.Slack'], socket, timeout=1800)
    # Flatpak source/GPG behavior comes from its ordinary signed remote; no
    # --no-gpg-verify, host installation, profile import or external login.
    # --product-version executes Chrome itself. Cache this public value before
    # arming tracing, so metadata reads cannot count as application launches.
    chrome_version = user(account, ['/usr/bin/google-chrome-stable', '--product-version'], socket).strip()
    guest.require(re.fullmatch(r'[0-9]+(?:\.[0-9]+){3}', chrome_version) is not None, 'invalid_chrome_version')
    version_path = guest.ROOT / 'desktop-chrome-version'
    version_path.write_text(chrome_version + '\n')
    version_path.chmod(0o600)
    slack_files = (guest.HOME / '.local/share/flatpak/app/com.slack.Slack/current/active/files').resolve()
    desktop_trace.install(run_id, slack_files=slack_files)
    values = versions(account, socket)
    remote = user(account, ['flatpak', 'remotes', '--user', '--columns=name,url'], socket)
    sources = [line.split('\t')[1] for line in remote.splitlines()
               if line.startswith('flathub\t') and len(line.split('\t')) == 2]
    guest.require(len(sources) == 1 and sources[0] == 'https://dl.flathub.org/repo/', 'unexpected_flatpak_source')
    assets = {'chrome': {'version': values['chrome'], 'source': 'https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb',
                         'sha256': chrome_sha256},
              'slack': {'version': values['slack'], 'origin': 'flathub', 'source': sources[0],
                        'ref': values['slack_ref'], 'commit': values['slack_commit']}}
    target = guest.ROOT / 'desktop-app-assets.json'
    guest.require(not target.exists(), 'desktop_assets_already_recorded')
    target.write_text(json.dumps(assets) + '\n')
    target.chmod(0o600)
    return {'installed': True, 'app_assets': assets}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['install', 'phases', 'versions', 'probe', 'gui-open', 'gui-click'])
    parser.add_argument('--chrome-sha256')
    parser.add_argument('--checkpoint')
    parser.add_argument('--action', choices=['cancel', 'approve', 'ui-input', 'not-exposed'])
    parser.add_argument('--role', choices=['chrome', 'slack'], default='chrome')
    parser.add_argument('--approve', action='store_true')
    parser.add_argument('--visually-inspected', action='store_true')
    parser.add_argument('--x', type=int)
    parser.add_argument('--y', type=int)
    args = parser.parse_args()
    run_id, account = guest.guard()
    if args.command == 'install':
        print(json.dumps(install_apps(run_id, account, args.chrome_sha256)))
        return
    if args.command == 'phases':
        import desktop_phases
        print(json.dumps(desktop_phases.run(run_id, account, checkpoint_id=args.checkpoint, action=args.action,
                                            x=args.x, y=args.y, inspected=args.visually_inspected)))
        return
    socket = guest.guest_socket(account)
    if args.command == 'versions':
        result = versions(account, socket)
    elif args.command == 'probe':
        result = probe(account, socket)
    elif args.command == 'gui-open':
        command = ['app', 'register-focused']
        if args.role == 'chrome':
            command += ['--desktop-id', DESKTOP_ID]
        open_approval(account, socket, command)
        result = {'surface_ready': True}
    else:
        result = register_action(run_id, account, socket, args.role, approve=args.approve,
                                 x=args.x, y=args.y, inspected=args.visually_inspected)
    print(json.dumps(result))


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.TimeoutExpired):
        # Raw subprocess errors may contain browser output. Never export them.
        print(json.dumps({'failed': True, 'reason': 'desktop_guest_prerequisite_or_assertion'}))
        raise SystemExit(1)
