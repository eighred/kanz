"""Byte-bounded CLI capture; stderr is counted but never retained or emitted."""
import subprocess
import threading
import os
import signal


def run(argv, *, cwd, env, data=None, timeout=100, stdout_limit=4 << 20, stderr_limit=65536):
    input_bytes = None if data is None else data.encode()
    if input_bytes is not None and len(input_bytes) > 4 << 20:
        raise ValueError("command input exceeds bound")
    process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=os.name == "posix")
    failed = threading.Event()
    output = bytearray()

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
        try:
            while chunk := pipe.read(65536):
                size += len(chunk)
                if size > limit:
                    stop()
                    return
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
            raise RuntimeError("bounded command failed or exceeded output limits")
        return output.decode("utf-8").strip()
    finally:
        if process.poll() is None or any(thread.is_alive() for thread in readers):
            stop()
            process.wait(timeout=5)
        for thread in readers:
            if thread.ident is not None:
                thread.join(timeout=5)
        if any(thread.is_alive() for thread in readers):
            raise RuntimeError("owned command streams did not terminate")
