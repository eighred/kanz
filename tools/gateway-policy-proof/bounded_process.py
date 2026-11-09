"""Byte-bounded CLI capture with optional fixed failure categories."""
import subprocess
import threading
import os
import signal


def run(argv, *, cwd, env, data=None, timeout=100, stdout_limit=4 << 20, stderr_limit=65536, strip=True, failure_signals=()):
    input_bytes = None if data is None else data.encode()
    if input_bytes is not None and len(input_bytes) > 4 << 20:
        raise ValueError("command input exceeds bound")
    process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=os.name == "posix")
    failed = threading.Event()
    output = bytearray()
    signals = set()
    overlap = max((len(marker) - 1 for marker, _ in failure_signals), default=0)

    def stop():
        failed.set()
        try:
            if os.name == "posix":
                os.killpg(process.pid, signal.SIGKILL)
            else:
                process.kill()
        except OSError:
            pass

    def read(pipe, limit, retain):
        size = 0
        tail = b""
        try:
            while chunk := pipe.read(65536):
                size += len(chunk)
                if size > limit:
                    stop()
                    return
                if failure_signals:
                    window = tail + chunk.lower()
                    signals.update(category for marker, category in failure_signals if marker in window)
                    tail = window[-overlap:] if overlap else b""
                if retain:
                    output.extend(chunk)
        except OSError:
            stop()
        finally:
            pipe.close()

    def write():
        try:
            if input_bytes is not None:
                process.stdin.write(input_bytes)
        except (BrokenPipeError, OSError):
            stop()
        finally:
            try:
                process.stdin.close()
            except OSError:
                stop()

    readers = [threading.Thread(target=read, args=(process.stdout, stdout_limit, True), daemon=True),
               threading.Thread(target=read, args=(process.stderr, stderr_limit, False), daemon=True),
               threading.Thread(target=write, daemon=True)]
    try:
        for thread in readers:
            thread.start()
        try:
            status = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            stop()
            process.wait(timeout=5)
            raise RuntimeError("bounded command deadline exceeded") from None
        for thread in readers:
            thread.join(timeout=5)
        if failed.is_set() or any(thread.is_alive() for thread in readers) or status:
            detail = " (" + ", ".join(sorted(signals)) + ")" if signals else ""
            raise RuntimeError("bounded command failed or exceeded output limits" + detail)
        decoded = output.decode("utf-8")
        return decoded.strip() if strip else decoded
    finally:
        if process.poll() is None or any(thread.is_alive() for thread in readers):
            stop()
            process.wait(timeout=5)
        for thread in readers:
            if thread.ident is not None:
                thread.join(timeout=5)
        if any(thread.is_alive() for thread in readers):
            raise RuntimeError("owned command streams did not terminate")
