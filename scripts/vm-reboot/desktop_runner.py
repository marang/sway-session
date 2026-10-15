"""Opt-in desktop transport and private visual-controller checkpoints.

Only schema-validated structural reports enter the evidence directory. Images,
keys, disks and diagnostic streams remain in disposable private directories.
"""
import json
import os
import platform
import re
import shlex
import shutil
import sys
import tempfile
import time
import uuid
from pathlib import Path

from desktop_evidence import (LIMIT, validate_desktop_baseline,
                              validate_desktop_evidence, validate_desktop_observation)


def read_report(data, *, limit=LIMIT):
    def pairs(rows):
        result = {}
        for key, value in rows:
            if key in result:
                raise ValueError('duplicate field')
            result[key] = value
        return result
    def constant(value):
        raise ValueError('nonfinite scalar')
    try:
        if not isinstance(data, (bytes, str)) or len(data.encode('utf-8') if isinstance(data, str) else data) > limit:
            raise ValueError('size limit')
        return json.loads(data, object_pairs_hook=pairs, parse_constant=constant, parse_float=constant)
    except (ValueError, TypeError, RecursionError):
        raise RuntimeError('desktop report: malformed or oversized JSON') from None


def read_report_file(path, *, limit=LIMIT):
    with Path(path).open('rb') as source:
        return read_report(source.read(limit + 1), limit=limit)


def failure_diagnostic(value):
    modules = {'guest', 'transport', 'desktop_guest', 'desktop_trace', 'desktop_pointer',
               'desktop_lifecycle', 'desktop_phases', 'desktop_boot', 'desktop_environment', 'desktop_evidence'}
    kinds = {'RuntimeError', 'OSError', 'ValueError', 'KeyError', 'TimeoutExpired'}
    if (type(value) is not dict or set(value) != {'version', 'kind', 'frames'}
            or type(value['version']) is not int or value['version'] != 1
            or type(value['kind']) is not str or value['kind'] not in kinds
            or type(value['frames']) is not list or len(value['frames']) > 6):
        raise RuntimeError('desktop diagnostic: invalid shape')
    for frame in value['frames']:
        if (type(frame) is not dict or set(frame) != {'module', 'line'}
                or type(frame['module']) is not str or frame['module'] not in modules
                or type(frame['line']) is not int or not 1 <= frame['line'] <= 10000):
            raise RuntimeError('desktop diagnostic: invalid frame')
    return value


def checkpoint_request(value):
    allowed = {'cancel', 'approve', 'ui-input', 'not-exposed'}
    if (type(value) is not dict or set(value) != {'id', 'kind', 'private_screenshot', 'actions'}
            or type(value['id']) is not str or not re.fullmatch(r'[a-z0-9][a-z0-9_.-]{0,63}', value['id'])
            or value['kind'] not in ('approval', 'capability')
            or type(value['actions']) is not list or not 1 <= len(value['actions']) <= 4
            or any(type(action) is not str or action not in allowed for action in value['actions'])
            or len(set(value['actions'])) != len(value['actions'])
            or type(value['private_screenshot']) is not str
            or not re.fullmatch(r'/home/ss-test/\.cache/lab253/[a-z0-9_.-]{1,64}\.png', value['private_screenshot'])):
        raise RuntimeError('desktop controller: invalid checkpoint')
    return value


def checkpoint_response(value, checkpoint):
    if (type(value) is not dict or not {'checkpoint', 'action'} <= value.keys()
            or not value.keys() <= {'checkpoint', 'action', 'x', 'y'}
            or value['checkpoint'] != checkpoint['id']
            or value['action'] not in checkpoint['actions']):
        raise RuntimeError('desktop controller: invalid response')
    coordinates = {'x', 'y'} <= value.keys()
    needs_pointer = (value['action'] in ('cancel', 'ui-input')
                     or (value['action'] == 'approve' and checkpoint['kind'] == 'approval'))
    if ('x' in value) != ('y' in value) or needs_pointer != coordinates:
        raise RuntimeError('desktop controller: invalid pointer response')
    if coordinates and not (type(value['x']) is int and 0 <= value['x'] < 1280
                            and type(value['y']) is int and 0 <= value['y'] < 720):
        raise RuntimeError('desktop controller: pointer outside fixture output')
    return value


def collect_phases(runner, guest, control, run_id):
    operation = 'python3 /opt/sway-session-test/desktop_guest.py phases'
    value = read_report(guest.ssh(operation, timeout=600))
    for _ in range(64):
        if type(value) is dict and set(value) == {'complete', 'phases', 'expected_ids'} and value['complete'] is True:
            ids = value['expected_ids']
            runner.require(type(ids) is list and len(ids) == 2 and len(set(ids)) == 2
                           and all(type(item) is str and str(uuid.UUID(item)) == item for item in ids),
                           'desktop phases returned invalid context identities')
            return value
        runner.require(type(value) is dict and set(value) == {'complete', 'checkpoint'}
                       and value['complete'] is False, 'desktop phases returned invalid progress')
        checkpoint = checkpoint_request(value['checkpoint'])
        image = guest.ssh('cat -- ' + shlex.quote(checkpoint['private_screenshot']), timeout=15)
        runner.require(image.startswith(b'\x89PNG\r\n\x1a\n') and len(image) <= runner.LIMIT,
                       'desktop checkpoint requires a bounded PNG')
        for name in ['request.json', 'response.json', 'screen.png']:
            (control / name).unlink(missing_ok=True)
        (control / 'screen.png').write_bytes(image)
        runner.save(control / 'request.json', {'run_id': run_id, 'checkpoint': checkpoint, 'screenshot': 'screen.png'})
        print('Desktop visual checkpoint: ' + checkpoint['id'] + ' — ' + str(control / 'request.json'), flush=True)
        deadline = time.monotonic() + 1800
        while not (control / 'response.json').exists():
            runner.require(guest.process.poll() is None, 'owned desktop guest exited')
            runner.require(time.monotonic() < deadline, 'desktop visual checkpoint deadline exceeded')
            time.sleep(1)
        response = checkpoint_response(read_report_file(control / 'response.json', limit=4096), checkpoint)
        argv = ['--checkpoint', response['checkpoint'], '--action', response['action'], '--visually-inspected']
        if 'x' in response:
            argv.extend(['--x', str(response['x']), '--y', str(response['y'])])
        value = read_report(guest.ssh(operation + ' ' + shlex.join(argv), timeout=600))
    raise RuntimeError('desktop controller: checkpoint budget exceeded')


def run_desktop(runner, args):
    if args.check_evidence:
        print(json.dumps(validate_desktop_evidence(read_report_file(args.check_evidence)), indent=2))
        return
    runner.require(args.desktop_control is not None, 'desktop scenario requires --desktop-control for actual visual checkpoints')
    runner.require(sys.platform == 'linux' and platform.machine() == 'x86_64', 'desktop runner requires Linux x86_64')
    runner.require(os.access('/dev/kvm', os.R_OK | os.W_OK), 'KVM is unavailable; desktop scenario not run')
    for name in ['qemu-system-x86_64', 'qemu-img', 'cloud-localds', 'ssh', 'ssh-keygen', 'go', 'curl']:
        runner.require(shutil.which(name), 'required desktop VM tool unavailable: ' + name)
    os.umask(0o077)
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix='ss-desktop-evidence-'))
    control = args.desktop_control.resolve()
    runner.require(not output.is_relative_to(runner.REPO) and not control.is_relative_to(runner.REPO)
                   and not control.is_relative_to(output) and not output.is_relative_to(control),
                   'desktop evidence and private control require separate directories outside checkout')
    runner.require(not control.exists(), 'desktop control directory must be new')
    if args.output:
        output.mkdir(mode=0o700)
    control.mkdir(mode=0o700)
    root = Path(tempfile.mkdtemp(prefix='ss-desktop-kvm-'))
    run_id = str(uuid.uuid4())
    guest, failure, cleanup_ok, stage, diagnostic = None, False, True, 'prepare', None
    evidence = {'version': 1, 'scenario': 'desktop', 'source': 'qemu-kvm', 'run_id': run_id,
                'observations': [], 'reboot_requests': []}
    print('Desktop VM evidence: ' + str(output), flush=True)
    try:
        assets = read_report_file(runner.FIXTURE / 'assets.json')
        image, herdr, candidate, observer = runner.prepare_artifacts(args, root, assets, evidence)
        chrome = args.chrome.resolve() if args.chrome else runner.fetch(assets['chrome'], root / 'chrome.deb', 256 * 1024**2)
        runner.require(runner.digest(chrome) == assets['chrome']['sha256'], 'Chrome package digest differs from manifest')
        stage = 'provision'
        guest = runner.Guest(root, image, run_id, scenario='desktop')
        guest.start(); guest.wait()
        guest.ssh('cloud-init status --wait', timeout=1800)
        runner.require(runner.qmp(root / 'qmp.sock', 'query-kvm')['enabled'] is True, 'desktop guest requires KVM')
        for local, name in [(candidate, 'sway-session'), (observer, 'observe'), (herdr, 'herdr'), (chrome, 'google-chrome.deb')]:
            guest.upload(local, '/opt/sway-session-test/' + name)
        for name in ['guest.py', 'transport.py', 'desktop_environment.py', 'desktop_evidence.py', 'desktop_guest.py', 'desktop_trace.py',
                     'desktop_pointer.py', 'desktop_lifecycle.py', 'desktop_phases.py', 'desktop_boot.py']:
            guest.upload(runner.FIXTURE / name, '/opt/sway-session-test/' + name)
        guest.ssh('cd /opt/sway-session-test && python3 -c ' + shlex.quote(
            'import guest; from pathlib import Path; r,a=guest.guard(); guest.install(r,a,candidate=Path("/usr/bin/sway-session"),desktop=True)'), timeout=120)
        installed = read_report(guest.ssh('python3 /opt/sway-session-test/desktop_guest.py install --chrome-sha256 ' + assets['chrome']['sha256'], timeout=2400))
        runner.require(type(installed) is dict and set(installed) == {'installed', 'app_assets'} and installed['installed'] is True,
                       'desktop apps were not installed')
        evidence['app_assets'] = installed['app_assets']
        # Cloud-init upgrades packages, including the kernel. Settle that OS
        # transition before taking the immutable acceptance baseline.
        stage = 'setup_reboot'
        evidence['setup_reboot'] = runner.request_reboot(guest, guest.wait(), 0)
        stage = 'phases'
        phases = collect_phases(runner, guest, control, run_id)
        evidence['phases'] = phases['phases']
        stage = 'baseline'
        baseline = read_report(guest.ssh('python3 /opt/sway-session-test/desktop_boot.py baseline --expected ' + ','.join(phases['expected_ids']), timeout=240))
        validate_desktop_baseline(baseline)
        validate_desktop_observation(baseline, baseline, run_id, evidence['artifact'])
        evidence['observations'].append(baseline)
        runner.save(output / 'before.json', baseline)
        for index in [1, 2]:
            stage = 'reboot' + str(index)
            request = runner.request_reboot(guest, evidence['observations'][-1]['boot_id'], index)
            evidence['reboot_requests'].append(request)
            observation = read_report(guest.ssh('python3 /opt/sway-session-test/desktop_boot.py observe', timeout=240))
            # Preserve only independently schema-validated structural evidence
            # if the subsequent comparison with the original baseline fails.
            validate_desktop_baseline(observation)
            runner.save(output / ('after' + str(index) + '.json'), observation)
            runner.require(observation['boot_id'] == request['after_boot_id'], 'desktop boot attribution differs')
            validate_desktop_observation(observation, baseline, run_id, evidence['artifact'])
            evidence['observations'].append(observation)
            print('Desktop guest reboot ' + str(index) + ': original Follow layout and measured launches verified', flush=True)
        stage = 'validate'
    except (Exception, KeyboardInterrupt):
        failure = True
        if guest:
            try:
                diagnostic = failure_diagnostic(read_report(guest.ssh(
                    'cat /opt/sway-session-test/desktop-failure.json', timeout=5), limit=4096))
            except (Exception, KeyboardInterrupt):
                pass
    finally:
        try:
            if guest:
                guest.close()
        except (Exception, KeyboardInterrupt):
            cleanup_ok = False
        for directory in [root, control]:
            try:
                shutil.rmtree(directory)
            except (Exception, KeyboardInterrupt):
                cleanup_ok = False
    evidence['cleanup'] = {'vm_closed': cleanup_ok, 'temporary_state_removed': cleanup_ok}
    if failure or not cleanup_ok:
        report = {'version': 1, 'scenario': 'desktop', 'run_id': run_id,
                    'reason': 'desktop_setup_or_assertion_failed' if failure else 'owned_cleanup_failed',
                    'stage': stage, 'cleanup': evidence['cleanup']}
        if diagnostic is not None:
            report['diagnostic'] = diagnostic
        runner.save(output / 'failure.json', report)
        raise RuntimeError('desktop scenario failed; typed failure report: ' + str(output / 'failure.json')) from None
    try:
        evidence['checks'] = validate_desktop_evidence(evidence)
    except (Exception, KeyboardInterrupt):
        runner.save(output / 'failure.json', {'version': 1, 'scenario': 'desktop', 'run_id': run_id,
                    'reason': 'desktop_evidence_rejected', 'stage': 'validate', 'cleanup': evidence['cleanup']})
        raise RuntimeError('desktop evidence rejected after owned cleanup') from None
    runner.save(output / 'result.json', evidence)
    print(json.dumps(evidence['checks'], indent=2), flush=True)
