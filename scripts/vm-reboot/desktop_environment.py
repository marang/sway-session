#!/usr/bin/env python3
"""Publish only disposable Sway session routing, never a process environment."""
import json
import os
import pwd
import re
import stat
import tempfile
from pathlib import Path


def publish():
    account = pwd.getpwnam('ss-test')
    home = Path('/home/ss-test')
    if os.geteuid() != account.pw_uid or account.pw_dir != str(home):
        raise RuntimeError('fixture environment publisher requires ss-test')
    directory = home / '.local/state'
    info = directory.lstat()
    if directory.resolve() != directory or not stat.S_ISDIR(info.st_mode) or info.st_uid != account.pw_uid or info.st_mode & 0o077:
        raise RuntimeError('fixture environment directory is not private')
    address = os.environ.get('DBUS_SESSION_BUS_ADDRESS', '').split(',guid=', 1)[0]
    display = os.environ.get('DISPLAY', '')
    if not re.fullmatch(r'unix:path=/(?:tmp/dbus-[A-Za-z0-9]+|run/user/[0-9]+/bus)', address):
        raise RuntimeError('unsupported private session bus address')
    if display and not re.fullmatch(r':[0-9]{1,4}', display):
        raise RuntimeError('unsupported fixture X display')
    value = {'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
             'socket': os.environ['SWAYSOCK'],
             'environment': {'DBUS_SESSION_BUS_ADDRESS': address,
                             'XDG_CURRENT_DESKTOP': 'sway', 'XDG_SESSION_TYPE': 'wayland'}}
    if display:
        value['environment']['DISPLAY'] = display
    descriptor, temporary = tempfile.mkstemp(prefix='.fixture-env-', dir=directory)
    try:
        with os.fdopen(descriptor, 'w') as stream:
            json.dump(value, stream)
        os.replace(temporary, directory / 'fixture-session-env.json')
    finally:
        Path(temporary).unlink(missing_ok=True)


if __name__ == '__main__':
    publish()
