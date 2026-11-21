"""Isolated OpenCL comparison worker. Only the fixed positive-time FP64 workload is accepted."""
import json
import math
import sys
from pathlib import Path

import numpy as np
import pyopencl as cl


def main(device_name):
    devices = [d for p in cl.get_platforms() for d in p.get_devices()
               if d.type & cl.device_type.GPU and d.name == device_name]
    if len(devices) != 1 or not devices[0].double_fp_config:
        raise ValueError("expected one selected FP64 GPU")
    device = devices[0]
    context = cl.Context([device])
    queue = cl.CommandQueue(context, properties=cl.command_queue_properties.PROFILING_ENABLE)
    source = Path(__file__).with_name("american.cl").read_text(encoding="utf-8")
    program = cl.Program(context, source).build(options=["-cl-std=CL1.2"])
    kernel = cl.Kernel(program, "price")
    inputs = cl.Buffer(context, cl.mem_flags.READ_ONLY, 8 * 7 * 8)
    outputs = cl.Buffer(context, cl.mem_flags.WRITE_ONLY, 8 * 8)
    for line in sys.stdin:
        rows = np.asarray(json.loads(line), dtype=np.float64)
        if rows.shape != (8, 7) or not np.isfinite(rows).all():
            raise ValueError("invalid reference batch")
        for sign, spot, strike, time, rate, dividend, vol in rows:
            if sign not in (-1, 1) or min(spot, strike, time, vol) <= 0:
                raise ValueError("unsupported diagnostic input")
            dt = time / 200
            step = vol * math.sqrt(dt)
            if abs((rate - dividend) * dt) > step:
                raise ValueError("inadmissible lattice")
        upload = cl.enqueue_copy(queue, inputs, rows)
        compute = kernel(queue, (8 * 256,), (256,), inputs, outputs)
        result = np.empty(8, dtype=np.float64)
        download = cl.enqueue_copy(queue, result, outputs, is_blocking=True)
        print(json.dumps({
            "prices": result.tolist(),
            "device": {"name": device.name, "vendor": device.vendor,
                       "driver": device.driver_version, "opencl": device.version,
                       "global_mem_bytes": device.global_mem_size,
                       "pyopencl": cl.VERSION_TEXT, "numpy": np.__version__},
            "device_ms": {name: (event.profile.end - event.profile.start) / 1e6
                          for name, event in (("upload", upload), ("kernel", compute),
                                              ("download", download))},
        }, allow_nan=False), flush=True)


if __name__ == "__main__":
    main(sys.argv[1])
