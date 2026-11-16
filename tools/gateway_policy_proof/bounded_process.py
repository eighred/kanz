"""Byte-bounded CLI capture; stderr is counted but never retained or emitted."""
import subprocess
import threading
import os
import signal
import time


class _CommandObservation:
    """Private scalar payload; it never retains command, input or output bytes."""
    def __init__(self, **values):
        self.values = tuple(values.items())


def command_failure(error):
    value = getattr(error, "_bounded_command_observation", None)
    if type(value) is not _CommandObservation:
        return None
    result = dict(value.values)
    integers = {"timeoutUs", "elapsedUs", "stdoutBytes", "stderrBytes", "stdoutLimitBytes", "stderrLimitBytes", "joinedStreams"}
    booleans = {"launched", "killRequested", "reaped", "cleanupFailed"}
    if (set(result) != integers | booleans | {"outcome", "exitCode", "signal"}
            or type(result["outcome"]) is not str
            or result["outcome"] not in {"launch_error", "deadline", "nonzero", "stdout_limit", "stderr_limit", "stream_error", "stream_join_timeout", "failure"}
            or any(type(result[key]) is not int or not 0 <= result[key] <= 2100000000 for key in integers)
            or any(type(result[key]) is not bool for key in booleans)
            or any(result[key] is not None and (type(result[key]) is not int or not 0 <= result[key] <= 255) for key in ("exitCode", "signal"))):
        raise ValueError("invalid bounded command observation")
    return result


def run(argv, *, cwd, env, data=None, timeout=100, stdout_limit=4 << 20, stderr_limit=65536, strip=True):
    input_bytes = None if data is None else data.encode()
    if input_bytes is not None and len(input_bytes) > 4 << 20:
        raise ValueError("command input exceeds bound")
    started = time.monotonic()
    try:
        process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=os.name == "posix")
    except OSError as error:
        error._bounded_command_observation = _CommandObservation(
            outcome="launch_error", timeoutUs=int(timeout * 1000000),
            elapsedUs=int((time.monotonic() - started) * 1000000),
            stdoutBytes=0, stderrBytes=0, stdoutLimitBytes=stdout_limit,
            stderrLimitBytes=stderr_limit, launched=False, killRequested=False,
            reaped=False, joinedStreams=0, cleanupFailed=False, exitCode=None, signal=None)
        raise
    failed = threading.Event()
    output = bytearray()
    counts = [0, 0]
    reason = ["failure"]
    kill_requested = False
    reaped = False
    primary = None
    cleanup_failed = False

    def wait(duration):
        nonlocal reaped
        status = process.wait(timeout=duration)
        reaped = True
        return status

    def stop(cause=None):
        nonlocal kill_requested
        if cause is not None and not failed.is_set():
            reason[0] = cause
        failed.set()
        kill_requested = True
        try:
            if os.name == "posix":
                os.killpg(process.pid, signal.SIGKILL)
            else:
                process.kill()
        except OSError:
            pass

    def read(pipe, limit, retain, index):
        size = 0
        try:
            while chunk := pipe.read(65536):
                size += len(chunk)
                counts[index] = size
                if size > limit:
                    stop("stdout_limit" if retain else "stderr_limit")
                    return
                if retain:
                    output.extend(chunk)
        except OSError:
            stop("stream_error")
        finally:
            pipe.close()

    def write():
        try:
            if input_bytes is not None:
                process.stdin.write(input_bytes)
        except (BrokenPipeError, OSError):
            stop("stream_error")
        finally:
            try:
                process.stdin.close()
            except OSError:
                stop("stream_error")

    readers = [threading.Thread(target=read, args=(process.stdout, stdout_limit, True, 0), daemon=True),
               threading.Thread(target=read, args=(process.stderr, stderr_limit, False, 1), daemon=True),
               threading.Thread(target=write, daemon=True)]
    joined = [False] * len(readers)

    def join(index):
        readers[index].join(timeout=5)
        if not readers[index].is_alive():
            joined[index] = True

    try:
        for thread in readers:
            thread.start()
        try:
            status = wait(timeout)
        except subprocess.TimeoutExpired:
            reason[0] = "deadline"
            primary = RuntimeError("bounded command deadline exceeded")
            stop()
            try:
                wait(5)
            except Exception:
                cleanup_failed = True
            raise primary from None
        for index in range(len(readers)):
            join(index)
        if failed.is_set() or any(thread.is_alive() for thread in readers) or status:
            if not failed.is_set():
                reason[0] = "stream_join_timeout" if any(thread.is_alive() for thread in readers) else "nonzero"
            raise RuntimeError("bounded command failed or exceeded output limits")
        decoded = output.decode("utf-8")
        return decoded.strip() if strip else decoded
    except RuntimeError as error:
        primary = error
        raise
    finally:
        try:
            if process.poll() is None or any(thread.is_alive() for thread in readers):
                stop()
                wait(5)
            for index, thread in enumerate(readers):
                if thread.ident is not None:
                    join(index)
            if any(thread.is_alive() for thread in readers):
                raise RuntimeError("owned command streams did not terminate")
        except Exception as error:
            # Earlier failure remains primary. Joined/reaped facts still expose
            # incomplete cleanup; requesting SIGKILL proves no group absence.
            cleanup_failed = True
            if primary is None:
                primary = error
                reason[0] = "stream_join_timeout"
                raise
        finally:
            if primary is not None:
                status = process.returncode
                primary._bounded_command_observation = _CommandObservation(
                    outcome=reason[0], timeoutUs=int(timeout * 1000000),
                    elapsedUs=int((time.monotonic() - started) * 1000000),
                    stdoutBytes=counts[0], stderrBytes=counts[1], launched=True,
                    stdoutLimitBytes=stdout_limit, stderrLimitBytes=stderr_limit,
                    killRequested=kill_requested, reaped=reaped,
                    joinedStreams=sum(joined),
                    cleanupFailed=cleanup_failed,
                    exitCode=status if type(status) is int and status >= 0 else None,
                    signal=-status if type(status) is int and status < 0 else None)
