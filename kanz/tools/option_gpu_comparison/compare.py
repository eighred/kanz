"""Compare fresh-process and warm eight-price batches with the independent Snell oracle."""
import argparse
import concurrent.futures
import hashlib
import json
import math
import platform
import random
import subprocess
import sys
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


def sample(command, request, expected, bounds, previous, gpu):
    # A fresh worker receives the same complete batch twice. The first includes
    # process launch, imports and device setup. Both include IPC and validation.
    times = []
    device, phases = None, []
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
        started = time.perf_counter_ns()
        proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        try:
            for _ in range(2):
                proc.stdin.write(request)
                proc.stdin.flush()
                line = executor.submit(proc.stdout.readline).result(timeout=DEADLINE)
                response = json.loads(line)
                if gpu:
                    device = response["device"]
                    phases.append(response["device_ms"])
                    response = response["prices"]
                previous = check(response, expected, bounds, previous)
                times.append((time.perf_counter_ns() - started) / 1e6)
                started = time.perf_counter_ns()
            proc.stdin.close()
            if proc.wait(timeout=DEADLINE) != 0 or proc.stdout.read(1):
                raise RuntimeError("worker failed to finish cleanly")
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait()
            proc.stdout.close()
            proc.stdin.close()
    return times, previous, device, phases


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpu-worker", required=True, type=Path)
    parser.add_argument("--device", required=True)
    parser.add_argument("--samples", type=int, default=SAMPLES)
    args = parser.parse_args()
    if not 1 <= args.samples <= 1000:
        parser.error("sample count must be 1..1000")
    cpu = args.cpu_worker.resolve(strict=True)
    command = {"cpu": [str(cpu)], "gpu": [sys.executable, "-B", str(HERE / "gpu_worker.py"), args.device]}
    request, expected, bounds = workload()
    record = {
        "status": "NONFINAL", "source": subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "seed": SEED, "samples": args.samples, "device": args.device,
        "machine": platform.platform(), "cpu": platform.processor(),
        "python": sys.version, "cpu_worker_sha256": sha256(cpu),
        "request_sha256": hashlib.sha256(request).hexdigest(),
        "inputs_sha256": {p.name: sha256(p) for p in [
            ORACLE, HERE / "american.cl", HERE / "main.go", HERE / "gpu_worker.py", Path(__file__)]},
    }
    samples = {"cpu": [], "gpu": []}
    phases = []
    bits = {"cpu": [], "gpu": []}
    try:
        # Alternate the order to avoid consistently sampling one worker first.
        for index in range(args.samples):
            for worker in (("cpu", "gpu") if index % 2 == 0 else ("gpu", "cpu")):
                duration, bits[worker], device, device_times = sample(
                    command[worker], request, expected, bounds, bits[worker], worker == "gpu")
                if device:
                    if record.setdefault("gpu_runtime", device) != device:
                        raise ValueError("device changed between samples")
                    phases.append(device_times)
                samples[worker].append(duration)
        record["gpu_device_ms"] = phases
        record["timings_ms"] = samples
        record["prices_hex"] = bits
        p99 = {}
        for name, results in samples.items():
            rank = math.ceil(0.99 * len(results)) - 1
            p99[name] = {mode: sorted(row[i] for row in results)[rank]
                         for i, mode in enumerate(("cold", "warm"))}
        record["p99_ms"] = p99
        record["status"] = "DIAGNOSTIC_COMPLETE"
        record["latency_eligible"] = (args.samples >= SAMPLES
                                      and all(p99["gpu"][mode] < p99["cpu"][mode]
                                              for mode in ("cold", "warm")))
    except (OSError, RuntimeError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as exc:
        record["failure"] = type(exc).__name__
        print(json.dumps(record, allow_nan=False))
        return 1
    print(json.dumps(record, allow_nan=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
