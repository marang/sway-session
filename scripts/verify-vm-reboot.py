#!/usr/bin/env python3
"""Development-only KVM guest reboot acceptance; never targets a host session."""
import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
FIXTURE = REPO / 'scripts' / 'vm-reboot'
sys.path.insert(0, str(FIXTURE))
from transport import defer_start_signals, fetch_verified, run_command, start_bounded_log
LIMIT = 2 * 1024 * 1024


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    with open(path, 'rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def read_json(data):
    require(len(data) <= LIMIT, 'JSON exceeds evidence limit')
    return json.loads(data)


def validate_evidence(evidence):
    require(evidence['source'] == 'qemu-kvm', 'evidence source is not KVM')
    observations = evidence['observations']
    require(len(observations) == 3, 'expected baseline and two reboot observations')
    boots = [o['boot_id'] for o in observations]
    require(all(boots) and len(set(boots)) == 3, 'expected three distinct guest boot IDs')
    requests = evidence['reboot_requests']
    require(len(requests) == 2, 'expected two accepted guest reboot requests')
    for index, request in enumerate(requests):
        require(request['accepted'] is True and request['method'] == 'guest systemd timer -> systemctl reboot', 'guest reboot request was not accepted')
        require(request['before_boot_id'] == boots[index] and request['after_boot_id'] == boots[index + 1], 'reboot request does not match observed boot transition')
    baseline = observations[0]
    validate_baseline(baseline)
    for obs in observations:
        validate_observation(obs, baseline, evidence['run_id'], evidence['artifact'])
    return {'passed': True, 'guest_reboots': 2, 'target_workspace': '98', 'managed_windows': 2,
            'production_notebook_tested': False,
            'excluded': ['agent conversation resume', 'pane working directory', 'focus', 'proportions', 'floating geometry', 'AppArmor enforcement']}


def validate_baseline(baseline):
    require(len(baseline['contexts']) == 2 and all(c['state'] == 'active' for c in baseline['contexts']), 'expected two active contexts')
    workspaces = baseline['live']['workspaces']
    require(len(workspaces) == 1 and workspaces[0]['name'] == '98', 'expected only workspace 98')
    ws = workspaces[0]
    tiling = ws.get('tiling') or {}
    require(ws['restore_mode'] == 'layout' and tiling.get('layout') == 'tabbed' and len(tiling.get('children', [])) == 2 and not ws.get('floating'), 'baseline needs two tiled windows in tabbed layout')
    ids = sorted(c['id'] for c in baseline['contexts'])
    require(ids == sorted(n.get('context_id', '') for n in tiling['children']) and len(set(ids)) == 2, 'layout and registry membership differ')


def validate_observation(obs, baseline, run_id, artifact):
    require(obs['run_id'] == run_id, 'guest run identity changed')
    require(obs['live'] == baseline['live'] and obs['stored'] == baseline['live'], 'restored ordered layout differs from original baseline')
    require(obs['contexts'] == baseline['contexts'], 'registered context membership or state changed')
    require(obs['staging_managed_windows'] == 0, 'managed window remains in staging')
    require(obs['daemon_binary_current'] is True, 'running daemon does not match candidate')
    require(obs['delay_inhibitor'] is True, 'shutdown delay inhibitor is not held')
    require(obs['artifact'] == artifact, 'candidate identity changed')


def command(argv, *, timeout=60, input=None, cwd=None, env=None):
    return run_command(argv, timeout=timeout, input=input, limit=LIMIT, cwd=cwd, env=env)


def fetch(asset, target, limit):
    return fetch_verified(asset, target, limit)


def qmp(path, name):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        peer.settimeout(10)
        peer.connect(str(path))
        with peer.makefile('rwb') as stream:
            require('QMP' in read_json(stream.readline(65537)), 'invalid QMP greeting')
            for request in ['qmp_capabilities', name]:
                stream.write(json.dumps({'execute': request, 'id': request}).encode() + b'\n')
                stream.flush()
                for _ in range(64):
                    reply = read_json(stream.readline(65537))
                    if reply.get('id') == request:
                        require('return' in reply, f'QMP {request} failed')
                        break
                else:
                    raise RuntimeError('QMP event budget exceeded')
            return reply['return']


def save(path, value):
    with open(path, 'x', encoding='utf-8') as dest:
        json.dump(value, dest, indent=2)
        dest.write('\n')
    path.chmod(0o600)


class Guest:
    def __init__(self, root, image, run_id):
        self.root, self.run_id = root, run_id
        command(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', root / 'key'])
        public = (root / 'key.pub').read_text().strip()
        config = {
            'disable_root': False, 'ssh_pwauth': False,
            'users': [{'name': 'root', 'ssh_authorized_keys': [public]}, {'name': 'ss-test', 'shell': '/bin/bash', 'lock_passwd': True}],
            'bootcmd': [['systemctl', 'mask', '--now', 'systemd-time-wait-sync.service']],
            'package_update': True, 'package_upgrade': True,
            'packages': ['sway', 'alacritty', 'mesa', 'ttf-dejavu', 'python', 'jq', 'sqlite', 'dbus', 'openssh', 'polkit'],
            'write_files': [{'path': '/opt/sway-session-test/run-id', 'permissions': '0600', 'content': run_id + '\n'}],
            'runcmd': [['systemctl', 'enable', '--now', 'sshd']],
        }
        (root / 'user-data').write_text('#cloud-config\n' + json.dumps(config))
        (root / 'meta-data').write_text(json.dumps({'instance-id': run_id, 'local-hostname': 'sway-session-test'}))
        command(['cloud-localds', root / 'seed.iso', root / 'user-data', root / 'meta-data'])
        command(['qemu-img', 'create', '-q', '-f', 'qcow2', '-F', 'qcow2', '-b', image, root / 'disk.qcow2'])
        command(['qemu-img', 'resize', '-q', root / 'disk.qcow2', '12G'])
        with socket.socket() as reserved:
            reserved.bind(('127.0.0.1', 0))
            self.port = reserved.getsockname()[1]
        self.ssh_flags = ['-F', '/dev/null', '-i', str(root / 'key'), '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
                          '-o', 'ConnectTimeout=5', '-o', 'ConnectionAttempts=1', '-o', 'StrictHostKeyChecking=accept-new',
                          '-o', f'UserKnownHostsFile={root / "known-hosts"}', '-o', 'LogLevel=ERROR']
        self.process = None
        self.log = None

    def start(self):
        root = self.root
        with defer_start_signals():
            self.process = subprocess.Popen([
            'qemu-system-x86_64', '-enable-kvm', '-machine', 'q35', '-cpu', 'host', '-m', '2048', '-smp', '2',
            '-drive', f'file={root / "disk.qcow2"},format=qcow2,if=virtio',
            '-drive', f'file={root / "seed.iso"},format=raw,if=virtio,readonly=on',
            '-netdev', f'user,id=net0,hostfwd=tcp:127.0.0.1:{self.port}-:22', '-device', 'virtio-net-pci,netdev=net0',
            '-display', 'none', '-serial', 'stdio', '-monitor', 'none',
            '-qmp', f'unix:{root / "qmp.sock"},server=on,wait=off',
            ], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            self.log = start_bounded_log(self.process.stdout, root / 'serial.log')

    def ssh(self, text, *, timeout=60, input=None):
        require(self.process.poll() is None, 'owned QEMU process exited')
        return command(['ssh', *self.ssh_flags, '-p', str(self.port), 'root@127.0.0.1', text], timeout=timeout, input=input)

    def wait(self, old_boot=None, timeout=900):
        deadline, last = time.monotonic() + timeout, 'guest not ready'
        while time.monotonic() < deadline:
            require(self.process.poll() is None, 'owned QEMU process exited; inspect VM failure log')
            try:
                identity = self.ssh('cat /opt/sway-session-test/run-id; systemd-detect-virt --vm; cat /proc/sys/kernel/random/boot_id', timeout=15).decode().splitlines()
                require(len(identity) == 3 and identity[0] == self.run_id and identity[1] == 'kvm', 'SSH target is not the owned KVM guest')
                if identity[2] != old_boot:
                    return identity[2]
            except (RuntimeError, subprocess.TimeoutExpired) as error:
                last = str(error)
            time.sleep(2)
        raise RuntimeError(f'guest boot deadline exceeded: {last}')

    def upload(self, local, target):
        require(target.startswith('/opt/sway-session-test/'), 'invalid fixed guest upload destination')
        self.ssh(f'cat > {target}', input=Path(local).read_bytes(), timeout=90)

    def close(self):
        if self.process is not None and self.process.poll() is None:
            try:
                qmp(self.root / 'qmp.sock', 'quit')
                self.process.wait(timeout=10)
            except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired):
                self.process.terminate()
                try:
                    self.process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    self.process.kill(); self.process.wait(timeout=10)
        if self.log is not None:
            self.log.close()


def request_reboot(guest, previous_boot, index):
    current_boot = guest.wait(old_boot=None, timeout=15)
    require(current_boot == previous_boot, 'guest rebooted before the requested reboot')
    # A guest-local timer acknowledges acceptance while SSH is still alive.
    guest.ssh(f'systemd-run --quiet --unit=sway-session-test-reboot-{index} --on-active=3s /usr/bin/systemctl reboot', timeout=15)
    next_boot = guest.wait(previous_boot, timeout=180)
    return {'accepted': True, 'method': 'guest systemd timer -> systemctl reboot',
            'before_boot_id': previous_boot, 'after_boot_id': next_boot}


def main():
    def terminate(signum, frame):
        raise KeyboardInterrupt('runner interrupted; cleaning owned VM')
    signal.signal(signal.SIGTERM, terminate)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check-evidence', type=Path, help='validate existing evidence; does not run or prove a new reboot')
    parser.add_argument('--image', type=Path, help='existing pinned Arch cloud image; otherwise download the manifest asset')
    parser.add_argument('--herdr', type=Path, help='existing real Herdr executable; otherwise download the manifest asset')
    parser.add_argument('--candidate', type=Path, help='existing candidate; otherwise build this checkout')
    parser.add_argument('--output', type=Path, help='new evidence directory outside the checkout')
    args = parser.parse_args()
    if args.check_evidence:
        print(json.dumps(validate_evidence(read_json(args.check_evidence.read_bytes())), indent=2)); return
    require(sys.platform == 'linux' and platform.machine() == 'x86_64', 'runner requires Linux x86_64')
    require(os.access('/dev/kvm', os.R_OK | os.W_OK), 'KVM is unavailable; scenario not run')
    for name in ['qemu-system-x86_64', 'qemu-img', 'cloud-localds', 'ssh', 'ssh-keygen', 'go', 'curl']:
        require(shutil.which(name), f'required VM tool unavailable: {name}; scenario not run')
    os.umask(0o077)
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix='sway-session-vm-evidence-'))
    require(not output.is_relative_to(REPO), 'evidence directory must be outside checkout')
    if args.output:
        output.mkdir(mode=0o700)
    print(f'VM evidence: {output}', flush=True)
    assets = json.loads((FIXTURE / 'assets.json').read_text())
    run_id = str(uuid.uuid4())
    root = Path(tempfile.mkdtemp(prefix='ss-kvm-'))
    guest = None
    evidence = {'source': 'qemu-kvm', 'run_id': run_id, 'observations': [], 'reboot_requests': []}
    try:
        image = args.image.resolve() if args.image else fetch(assets['arch'], root / 'arch.qcow2', 1024**3)
        require(digest(image) == assets['arch']['sha256'], 'Arch base image does not match pinned SHA-256')
        info = read_json(command(['qemu-img', 'info', '--output=json', '--backing-chain', image]))
        require(len(info) == 1 and info[0]['format'] == 'qcow2' and info[0]['virtual-size'] <= 64 * 1024**3, 'base image must be standalone bounded qcow2')
        herdr = args.herdr.resolve() if args.herdr else fetch(assets['herdr'], root / 'herdr', 256 * 1024**2)
        herdr.chmod(herdr.stat().st_mode | 0o100) if not args.herdr else None
        environment = os.environ.copy()
        environment.update({'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64'})
        observer = root / 'observe'
        command(['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(observer), './scripts/vm-reboot/observe'], cwd=REPO, env=environment, timeout=300)
        candidate = args.candidate.resolve() if args.candidate else root / 'sway-session'
        if not args.candidate:
            commit = command(['git', '-C', REPO, 'rev-parse', 'HEAD']).decode().strip()
            modified = bool(command(['git', '-C', REPO, 'status', '--porcelain', '--untracked-files=normal']).strip())
            flag = str(modified).lower()
            stamp = f'sway-session-build-v1|dev|{commit}|{flag}|end-sway-session-build-v1'
            ldflags = f'-s -w -buildid= -X main.version=dev -X main.commit={commit} -X main.modified={flag} -X github.com/marang/sway-session/internal/buildmetadata.Stamp={stamp}'
            command(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags', ldflags, '-o', str(candidate), './cmd/sway-session'], cwd=REPO, env=environment, timeout=300)
        build = read_json(command([candidate, '--json', 'version']))['build']
        evidence['artifact'] = {'sha256': digest(candidate), 'build': {k: build[k] for k in ['product_version', 'commit', 'modified']}}
        evidence['base_image_sha256'] = digest(image)
        evidence['herdr_sha256'] = digest(herdr)
        evidence['qemu_version'] = command(['qemu-system-x86_64', '--version']).decode().splitlines()[0]
        print('Booting owned KVM guest and provisioning Arch packages', flush=True)
        guest = Guest(root, image, run_id)
        guest.start()
        guest.wait()
        guest.ssh('cloud-init status --wait', timeout=1200)
        require(qmp(root / 'qmp.sock', 'query-kvm')['enabled'] is True, 'QEMU did not enable KVM')
        for local, target in [(candidate, 'sway-session'), (observer, 'observe'), (herdr, 'herdr'),
                              (FIXTURE / 'guest.py', 'guest.py'), (FIXTURE / 'transport.py', 'transport.py')]:
            guest.upload(local, '/opt/sway-session-test/' + target)
        guest.ssh('python3 /opt/sway-session-test/guest.py install', timeout=120)
        print('Creating baseline through real CLI and Sway IPC', flush=True)
        baseline = read_json(guest.ssh('python3 /opt/sway-session-test/guest.py baseline', timeout=180))
        validate_baseline(baseline)
        validate_observation(baseline, baseline, run_id, evidence['artifact'])
        evidence['observations'].append(baseline)
        save(output / 'before.json', baseline)
        for index in [1, 2]:
            print(f'Requesting regular guest reboot {index}/2', flush=True)
            old_boot = evidence['observations'][-1]['boot_id']
            request = request_reboot(guest, old_boot, index)
            evidence['reboot_requests'].append(request)
            next_boot = request['after_boot_id']
            deadline, last = time.monotonic() + 180, 'automatic restore not ready'
            previous_observation, stable_samples = None, 0
            while time.monotonic() < deadline:
                try:
                    observation = read_json(guest.ssh('python3 /opt/sway-session-test/guest.py observe', timeout=30))
                    # Check this against the original, before allowing a second reboot.
                    require(observation['boot_id'] == next_boot and next_boot != old_boot, 'guest observation boot does not match the new guest boot')
                    validate_observation(observation, baseline, run_id, evidence['artifact'])
                    stable_samples = stable_samples + 1 if observation == previous_observation else 1
                    previous_observation = observation
                    if stable_samples >= 3:
                        break
                    time.sleep(1)
                except (RuntimeError, subprocess.TimeoutExpired, KeyError, ValueError) as error:
                    last = str(error)
                    previous_observation, stable_samples = None, 0
                    time.sleep(2)
            else:
                raise RuntimeError(f'automatic restore deadline exceeded: {last}')
            evidence['observations'].append(observation)
            save(output / f'after{index}.json', observation)
            print(f'Guest reboot {index}: original ordered tabbed layout restored', flush=True)
        evidence['checks'] = validate_evidence(evidence)
        save(output / 'result.json', evidence)
        print(json.dumps(evidence['checks'], indent=2), flush=True)
    except (Exception, KeyboardInterrupt) as error:
        evidence['failure'] = str(error)
        save(output / 'failure.json', evidence)
        if guest:
            try:
                diagnostic = guest.ssh('tail -c 2097152 /home/ss-test/.local/state/sway-fixture.log', timeout=15)
                (output / 'guest.log').write_bytes(diagnostic)
            except (OSError, RuntimeError, subprocess.TimeoutExpired):
                # SSH may already be unavailable; retain the original failure.
                pass
            for name in ['serial.log', 'qemu.log']:
                path = root / name
                if path.exists():
                    with open(path, 'rb') as source:
                        source.seek(max(0, path.stat().st_size - LIMIT))
                        (output / name).write_bytes(source.read(LIMIT))
        raise
    finally:
        primary_error = sys.exc_info()[1]
        cleanup_errors = []
        try:
            if guest:
                guest.close()
        except (Exception, KeyboardInterrupt) as error:
            cleanup_errors.append({'operation': 'close guest', 'error': str(error)[:3000]})
        finally:
            try:
                shutil.rmtree(root)
            except (Exception, KeyboardInterrupt) as error:
                cleanup_errors.append({'operation': 'remove VM directory', 'error': str(error)[:3000]})
        if cleanup_errors:
            try:
                save(output / 'cleanup.json', {'run_id': run_id,
                     'failure': str(primary_error)[:3000] if primary_error is not None else None,
                     'cleanup_errors': cleanup_errors})
            except (Exception, KeyboardInterrupt) as error:
                cleanup_errors.append({'operation': 'write cleanup evidence', 'error': str(error)[:3000]})
            message = 'owned VM cleanup failed: ' + '; '.join(
                row['operation'] + ': ' + row['error'] for row in cleanup_errors)
            print(message, file=sys.stderr)
            if primary_error is not None:
                primary_error.add_note(message)
            else:
                raise RuntimeError(message)


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print(f'VM reboot test failed: {error}', file=sys.stderr)
        sys.exit(1)
