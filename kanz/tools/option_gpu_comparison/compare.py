"""Compare fresh-process and warm eight-price batches with the independent Snell oracle."""
import argparse
import concurrent.futures
import hashlib
import json
import math
import os
import platform
import random
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
ORACLE = ROOT / "rust/crates/option_pricing/tests/american_reference.json"
DEADLINE = 30
SEED = 1546
SAMPLES = 100


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def checkpoint(path, record):
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False, dir=path.parent) as out:
        temporary = Path(out.name)
        try:
            json.dump(record, out, allow_nan=False)
            out.flush()
            os.fsync(out.fileno())
        except BaseException:
            out.close()
            temporary.unlink(missing_ok=True)
            raise
    try:
        # Windows readers can briefly hold the old file without delete sharing.
        # Retain atomic replacement and fail if access does not recover.
        deadline = time.monotonic() + 1
        while True:
            try:
                os.replace(temporary, path)
                break
            except PermissionError as exc:
                if getattr(exc, "winerror", None) not in (5, 32) or time.monotonic() >= deadline:
                    raise
                time.sleep(0.025)
    finally:
        temporary.unlink(missing_ok=True)


def cpu_identity(cpu, source):
    info = json.loads(subprocess.check_output(["go", "version", "-m", "-json", str(cpu)]))
    flags = {item["Key"]: item["Value"] for item in info["Settings"]}
    toolchain, = [line.split()[1] for line in (ROOT / "kanz/go.mod").read_text().splitlines()
                  if line.startswith("toolchain ")]
    if (info["Path"] != "github.com/eighred/kanz/tools/option_gpu_comparison"
            or info["GoVersion"] != toolchain or flags.get("vcs.revision") != source
            or flags.get("vcs.modified") != "false" or flags.get("vcs") != "git"):
        raise ValueError("CPU worker source, package or toolchain mismatch")
    return {"package": info["Path"], "go": info["GoVersion"], "revision": flags["vcs.revision"]}


def workload():
    reference = json.loads(ORACLE.read_bytes())
    cases = reference["cases"]
    if (reference["method"] != "decimal_snell_80_110_v1" or reference["steps"] != 200
            or len(cases) != 8 or len({c["id"] for c in cases}) != 8):
        raise ValueError("incompatible reference corpus")
    sequence = list(range(8))
    random.Random(SEED).shuffle(sequence)
    rows, expected, bounds = [], [], []
    for index in sequence:
        case = cases[index]
        values = list(map(float, case["inputs"]))
        rows.append([{"call": 1, "put": -1}[case["right"]], *values])
        expected.append(float(case["values"][0]))
        bounds.append(8192 * sys.float_info.epsilon * max(1, abs(values[0]), abs(values[1])))
    return (json.dumps(rows, separators=(",", ":")) + "\n").encode(), expected, bounds


def check(prices, expected, bounds, previous):
    if len(prices) != len(expected) or any(not math.isfinite(got)
            or abs(got - want) > limit for got, want, limit in zip(prices, expected, bounds)):
        raise ValueError("incomplete or out-of-tolerance result")
    bits = [float(value).hex() for value in prices]
    if previous and bits != previous:
        raise ValueError("nondeterministic prices")
    return bits


def sample(command, request, expected, bounds, previous, gpu, observe):
    # A fresh worker receives the same complete batch twice. The first includes
    # process launch, imports and device setup. Both include IPC and validation.
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
        started = time.perf_counter_ns()
        proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        try:
            for mode in ("cold", "warm"):
                proc.stdin.write(request)
                proc.stdin.flush()
                line = executor.submit(proc.stdout.readline).result(timeout=DEADLINE)
                if not line:
                    status = proc.wait(timeout=DEADLINE)
                    raise subprocess.CalledProcessError(status, command[0])
                response = json.loads(line)
                device = phases = None
                if gpu:
                    device = response["device"]
                    phases = response["device_ms"]
                    response = response["prices"]
                previous = check(response, expected, bounds, previous)
                observe(mode, (time.perf_counter_ns() - started) / 1e6, previous, device, phases)
                started = time.perf_counter_ns()
            proc.stdin.close()
            status = proc.wait(timeout=DEADLINE)
            if status:
                raise subprocess.CalledProcessError(status, command[0])
            if proc.stdout.read(1):
                raise ValueError("worker emitted an extra response")
        except BrokenPipeError:
            status = proc.wait(timeout=DEADLINE)
            raise subprocess.CalledProcessError(status, command[0]) from None
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait()
            proc.stdout.close()
            try:
                proc.stdin.close()
            except BrokenPipeError:
                pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpu-worker", required=True, type=Path)
    parser.add_argument("--device", required=True)
    parser.add_argument("--output", required=True, type=Path, help="new receipt path outside the checkout")
    parser.add_argument("--samples", type=int, default=SAMPLES)
    args = parser.parse_args()
    if not 1 <= args.samples <= 1000:
        parser.error("sample count must be 1..1000")
    output = args.output.resolve()
    if output.is_relative_to(ROOT) or output.exists():
        parser.error("choose a new receipt file outside the checkout")
    cpu = args.cpu_worker.resolve(strict=True)
    command = {"cpu": [str(cpu)], "gpu": [sys.executable, "-B", str(HERE / "gpu_worker.py"), args.device]}
    request, expected, bounds = workload()
    record = {
        "status": "NONFINAL", "source": subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "seed": SEED, "samples": args.samples, "device": args.device,
        "machine": platform.platform(), "cpu": platform.processor(),
        "python": sys.version, "cpu_worker_sha256": sha256(cpu),
        "request_sha256": hashlib.sha256(request).hexdigest(), "batches": [],
        "inputs_sha256": {p.name: sha256(p) for p in [
            ORACLE, HERE / "american.cl", HERE / "main.go", HERE / "gpu_worker.py", Path(__file__)]},
    }
    bits = record["prices_hex"] = {"cpu": [], "gpu": []}
    location = {"phase": "setup"}
    output.open("x").close()
    checkpoint(output, record)
    try:
        record["cpu_build"] = cpu_identity(cpu, record["source"])
        if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT).strip():
            raise ValueError("benchmark requires a clean source revision")
        # Alternate the order to avoid consistently sampling one worker first.
        for index in range(args.samples):
            for worker in (("cpu", "gpu") if index % 2 == 0 else ("gpu", "cpu")):
                location = {"sample": index + 1, "backend": worker, "phase": "cold"}

                def observe(mode, elapsed, result_bits, device, device_times):
                    batch = {"sample": index + 1, "backend": worker, "phase": mode, "elapsed_ms": elapsed}
                    if device:
                        if record.setdefault("gpu_runtime", device) != device:
                            raise ValueError("device changed between samples")
                        batch["device_ms"] = device_times
                    bits[worker] = result_bits
                    record["batches"].append(batch)
                    checkpoint(output, record)
                    location["phase"] = "warm" if mode == "cold" else "exit"

                sample(command[worker], request, expected, bounds, bits[worker], worker == "gpu", observe)
        p99 = {}
        rank = math.ceil(0.99 * args.samples) - 1
        for name in ("cpu", "gpu"):
            p99[name] = {mode: sorted(b["elapsed_ms"] for b in record["batches"]
                                      if b["backend"] == name and b["phase"] == mode)[rank]
                         for mode in ("cold", "warm")}
        record["p99_ms"] = p99
        record["status"] = "DIAGNOSTIC_COMPLETE"
        record["latency_eligible"] = (args.samples >= SAMPLES
                                      and all(p99["gpu"][mode] < p99["cpu"][mode]
                                              for mode in ("cold", "warm")))
    except (OSError, RuntimeError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as exc:
        record["failure"] = {**location, "type": type(exc).__name__, "detail": str(exc) or "deadline exceeded"}
        if isinstance(exc, subprocess.CalledProcessError):
            record["failure"]["returncode"] = exc.returncode
        checkpoint(output, record)
        return 1
    checkpoint(output, record)
    return 0


if __name__ == "__main__":
    sys.exit(main())
