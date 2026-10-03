"""Bounded host subprocess and download transport for the disposable VM runner."""
import os
import hashlib
import re
import selectors
import signal
import subprocess
import threading
import time
from contextlib import contextmanager, nullcontext
from pathlib import Path

LIMIT = 2 * 1024 * 1024
CHUNK = 64 * 1024


@contextmanager
def defer_start_signals():
    """Defer main-thread startup cancellation until the caller owns its child.

    Temporary Python handlers reset on exec, leaving child signal masks intact.
    Ignored signals remain ignored. Worker threads cannot install handlers.
    """
    if threading.current_thread() is not threading.main_thread():
        raise RuntimeError('startup signal deferral requires the main thread')
    previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
    pending = {}

    def receive(signum, frame):
        pending.setdefault(signum, frame)

    try:
        for sig, handler in previous.items():
            if handler != signal.SIG_IGN:
                signal.signal(sig, receive)
        yield
    finally:
        try:
            signal.signal(signal.SIGINT, previous[signal.SIGINT])
        finally:
            signal.signal(signal.SIGTERM, previous[signal.SIGTERM])
        for sig, frame in pending.items():
            handler = previous[sig]
            if callable(handler):
                handler(sig, frame)
            elif handler != signal.SIG_IGN:
                raise KeyboardInterrupt(f'startup interrupted by {signal.Signals(sig).name}')


def _stop(process):
    """Kill the new session's process group, then reap its direct child."""
    if process.returncode is None:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait()


def run_command(argv, timeout=60, input=None, limit=LIMIT, cwd=None, env=None):
    """Return stdout; fail while either output stream exceeds its byte budget.

    Input is streamed without making another copy. Timeout or cancellation kills
    the owned process group and reaps the child before propagating the error.
    """
    if timeout <= 0 or limit < 0:
        raise ValueError('timeout must be positive and limit nonnegative')
    argv = [str(arg) for arg in argv]
    pending = memoryview(input) if input is not None else memoryview(b'')
    deadline = time.monotonic() + timeout
    output = {'stdout': bytearray(), 'stderr': bytearray()}
    process = None
    with selectors.DefaultSelector() as selector:
        try:
            guard = defer_start_signals() if threading.current_thread() is threading.main_thread() else nullcontext()
            with guard:
                process = subprocess.Popen(argv, stdin=subprocess.PIPE if pending else subprocess.DEVNULL,
                                           stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                           start_new_session=True, cwd=cwd, env=env)
            for stream, name in [(process.stdout, 'stdout'), (process.stderr, 'stderr')]:
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, name)
            if pending:
                os.set_blocking(process.stdin.fileno(), False)
                selector.register(process.stdin, selectors.EVENT_WRITE, 'stdin')
            while selector.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(argv, timeout, bytes(output['stdout']), bytes(output['stderr']))
                for key, _ in selector.select(remaining):
                    stream, name = key.fileobj, key.data
                    if name == 'stdin':
                        try:
                            count = os.write(stream.fileno(), pending[:CHUNK])
                            pending = pending[count:]
                        except BrokenPipeError:
                            pending = memoryview(b'')
                        except BlockingIOError:
                            continue
                        if not pending:
                            selector.unregister(stream)
                            stream.close()
                    else:
                        try:
                            chunk = os.read(stream.fileno(), CHUNK)
                        except BlockingIOError:
                            continue
                        if not chunk:
                            selector.unregister(stream)
                            stream.close()
                        else:
                            if len(output[name]) + len(chunk) > limit:
                                raise RuntimeError(f'command {name} output exceeds evidence limit ({limit} bytes)')
                            output[name].extend(chunk)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise subprocess.TimeoutExpired(argv, timeout, bytes(output['stdout']), bytes(output['stderr']))
            try:
                process.wait(timeout=remaining)
            except subprocess.TimeoutExpired:
                raise subprocess.TimeoutExpired(argv, timeout, bytes(output['stdout']), bytes(output['stderr'])) from None
        except BaseException:
            if process is not None:
                _stop(process)
            raise
        finally:
            if process is not None:
                for stream in [process.stdin, process.stdout, process.stderr]:
                    if stream is not None:
                        stream.close()
    if process.returncode:
        detail = output['stderr'].decode(errors='replace')[-3000:]
        raise RuntimeError(f'{Path(argv[0]).name} exited {process.returncode}: {detail}')
    return bytes(output['stdout'])


class _BoundedLog:
    def __init__(self, pipe, target, limit):
        self.pipe, self.limit = pipe, limit
        self.stop = threading.Event()
        self.error = None
        descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        self.sink = os.fdopen(descriptor, 'wb', buffering=0)
        self.thread = threading.Thread(target=self._pump, name='vm-log-pump', daemon=True)
        try:
            self.thread.start()
        except BaseException:
            self.sink.close()
            raise

    def _pump(self):
        retained = 0
        try:
            with self.sink, self.pipe, selectors.DefaultSelector() as selector:
                os.set_blocking(self.pipe.fileno(), False)
                selector.register(self.pipe, selectors.EVENT_READ)
                while not self.stop.is_set():
                    if not selector.select(0.1):
                        continue
                    try:
                        chunk = os.read(self.pipe.fileno(), CHUNK)
                    except BlockingIOError:
                        continue
                    if not chunk:
                        break
                    kept = chunk[:max(0, self.limit - retained)]
                    while kept:
                        count = self.sink.write(kept)
                        retained += count
                        kept = kept[count:]
        except Exception as error:
            self.error = error

    def join(self, timeout=10):
        """Wait for EOF after stopping QEMU; propagate log I/O failures."""
        self.thread.join(timeout)
        if self.thread.is_alive():
            raise TimeoutError('VM log pump did not reach EOF')
        if self.error is not None:
            raise RuntimeError(f'VM log pump failed: {self.error}') from self.error

    def close(self):
        """Stop draining and close the owned pipe and file within a short wait."""
        self.stop.set()
        self.join()


def start_bounded_log(pipe, target, limit=LIMIT):
    """Own a pipe and save its first limit bytes to a new private file.

    The background pump discards excess bytes until EOF. Stop/reap QEMU before
    join(); close() also permits abandoning a still-open stream during cleanup.
    """
    if limit < 0:
        raise ValueError('log limit must be nonnegative')
    return _BoundedLog(pipe, target, limit)


def fetch_verified(asset, target, limit):
    """Download an HTTPS SHA-pinned asset to a new private file in 600 seconds."""
    if limit <= 0:
        raise ValueError('download limit must be positive')
    if not re.fullmatch(r'[0-9a-f]{64}', asset['sha256']) or not asset['url'].startswith('https://'):
        raise ValueError('invalid pinned HTTPS asset')
    # Older curl cannot cap transfers whose Content-Length is unavailable.
    version = run_command(['curl', '--disable', '--version'], timeout=10)
    match = re.match(rb'curl (\d+)\.(\d+)\.(\d+)', version)
    if match is None or tuple(map(int, match.groups())) < (8, 4, 0):
        raise RuntimeError('curl 8.4.0 or newer is required for streaming download limits')
    target = Path(target)
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.close(descriptor)
    try:
        run_command(['curl', '--disable', '--fail', '--location', '--silent', '--show-error',
                     '--proto', '=https', '--proto-redir', '=https', '--max-time', '600',
                     '--max-filesize', str(limit), '--output', target, '--url', asset['url']], timeout=610)
        if target.stat().st_size > limit:
            raise RuntimeError('download exceeds asset limit')
        with target.open('rb') as source:
            actual = hashlib.file_digest(source, 'sha256').hexdigest()
        if actual != asset['sha256']:
            raise RuntimeError('download SHA-256 mismatch')
        return target
    except BaseException:
        target.unlink(missing_ok=True)
        raise
