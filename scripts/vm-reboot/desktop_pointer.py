#!/usr/bin/env python3
"""Hold one pointer capability in the disposable headless Sway seat.

This is a protocol client, never an approval presenter or callback substitute.
The request signatures are from the official wlr virtual-pointer v1 protocol:
https://github.com/swaywm/wlr-protocols/blob/master/unstable/wlr-virtual-pointer-unstable-v1.xml
No motion, button event, application argv, title, or content is read or sent.
"""
import argparse
import ctypes as c
import json
import os
import pwd
import re
import stat
import subprocess
import uuid
from pathlib import Path


def require(value):
    if not value:
        raise RuntimeError('owned_pointer_prerequisite')


def guard(run_id):
    require(str(uuid.UUID(run_id)) == run_id and uuid.UUID(run_id).version == 4)
    account = pwd.getpwnam('ss-test')
    require(os.getuid() == account.pw_uid > 0 and account.pw_dir == '/home/ss-test')
    require(subprocess.run(['/usr/bin/systemd-detect-virt', '--vm'], capture_output=True,
                           timeout=5, check=True).stdout.strip() == b'kvm')
    token = Path('/home/ss-test/.cache/lab253/pointer-owner.json')
    info = token.lstat()
    require(token.resolve() == token and stat.S_ISREG(info.st_mode) and info.st_uid == account.pw_uid
            and info.st_nlink == 1 and not info.st_mode & 0o077 and 0 < info.st_size < 1024)
    value = json.loads(token.read_text())
    require(set(value) == {'run_id', 'boot_id', 'socket'} and value['run_id'] == run_id
            and value['boot_id'] == Path('/proc/sys/kernel/random/boot_id').read_text().strip()
            and value['socket'] == os.environ.get('SWAYSOCK'))
    runtime = Path('/run/user') / str(account.pw_uid)
    require(Path(os.environ['XDG_RUNTIME_DIR']) == runtime and runtime.stat().st_uid == account.pw_uid)
    display = os.environ.get('WAYLAND_DISPLAY', '')
    require(re.fullmatch(r'wayland-[0-9]+', display) is not None)
    info = (runtime / display).lstat()
    require(stat.S_ISSOCK(info.st_mode) and info.st_uid == account.pw_uid)


class Interface(c.Structure):
    pass


class Message(c.Structure):
    _fields_ = [('name', c.c_char_p), ('signature', c.c_char_p),
                ('types', c.POINTER(c.POINTER(Interface)))]


Interface._fields_ = [('name', c.c_char_p), ('version', c.c_int), ('method_count', c.c_int),
                      ('methods', c.POINTER(Message)), ('event_count', c.c_int),
                      ('events', c.POINTER(Message))]


def hold():
    library = c.CDLL('libwayland-client.so.0')
    library.wl_display_connect.argtypes = [c.c_char_p]
    library.wl_display_connect.restype = c.c_void_p
    library.wl_display_roundtrip.argtypes = [c.c_void_p]
    library.wl_display_roundtrip.restype = c.c_int
    library.wl_display_dispatch.argtypes = [c.c_void_p]
    library.wl_display_dispatch.restype = c.c_int
    library.wl_proxy_marshal_flags.argtypes = [c.c_void_p, c.c_uint32, c.POINTER(Interface),
                                               c.c_uint32, c.c_uint32]
    library.wl_proxy_marshal_flags.restype = c.c_void_p
    library.wl_proxy_add_listener.argtypes = [c.c_void_p, c.c_void_p, c.c_void_p]
    library.wl_proxy_add_listener.restype = c.c_int
    seat_interface = Interface.in_dll(library, 'wl_seat_interface')
    registry_interface = Interface.in_dll(library, 'wl_registry_interface')
    pointer_methods = (Message * 9)(*[Message(name, signature, None) for name, signature in [
        (b'motion', b'uff'), (b'motion_absolute', b'uuuuu'), (b'button', b'uuu'),
        (b'axis', b'uuf'), (b'frame', b''), (b'axis_source', b'u'),
        (b'axis_stop', b'uu'), (b'axis_discrete', b'uufi'), (b'destroy', b'')]])
    pointer_interface = Interface(b'zwlr_virtual_pointer_v1', 1, 9, pointer_methods, 0, None)
    create_types = (c.POINTER(Interface) * 2)(c.pointer(seat_interface), c.pointer(pointer_interface))
    manager_methods = (Message * 2)(Message(b'create_virtual_pointer', b'?on', create_types),
                                    Message(b'destroy', b'', None))
    manager_interface = Interface(b'zwlr_virtual_pointer_manager_v1', 1, 2, manager_methods, 0, None)
    display = library.wl_display_connect(None)
    require(display)
    registry = library.wl_proxy_marshal_flags(display, 1, c.byref(registry_interface), 1, 0, c.c_void_p())
    require(registry)
    found = {}
    global_type = c.CFUNCTYPE(None, c.c_void_p, c.c_void_p, c.c_uint32, c.c_char_p, c.c_uint32)
    remove_type = c.CFUNCTYPE(None, c.c_void_p, c.c_void_p, c.c_uint32)

    @global_type
    def global_added(_data, registry, name, interface, version):
        if interface == manager_interface.name and version >= 1:
            found['manager'] = library.wl_proxy_marshal_flags(registry, 0, c.byref(manager_interface),
                                                             1, 0, c.c_uint32(name), interface,
                                                             c.c_uint32(1), c.c_void_p())

    @remove_type
    def global_removed(_data, _registry, _name):
        pass

    class Listener(c.Structure):
        _fields_ = [('global_added', global_type), ('global_removed', remove_type)]

    listener = Listener(global_added, global_removed)
    require(library.wl_proxy_add_listener(registry, c.byref(listener), None) == 0)
    require(library.wl_display_roundtrip(display) >= 0 and found.get('manager'))
    pointer = library.wl_proxy_marshal_flags(found['manager'], 0, c.byref(pointer_interface),
                                             1, 0, c.c_void_p(), c.c_void_p())
    require(pointer and library.wl_display_roundtrip(display) >= 0)
    while library.wl_display_dispatch(display) >= 0:
        pass
    raise RuntimeError('owned_pointer_disconnected')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-id', required=True)
    args = parser.parse_args()
    guard(args.run_id)
    hold()
