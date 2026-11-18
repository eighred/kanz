# American option GPU comparison

This disposable workload prices the eight American contracts in the [independent Decimal Snell reference](../../../rust/crates/option_pricing/tests/american_reference.json). It compares the existing Go `pricing.Price` call (one goroutine per contract) with an FP64 OpenCL 1.2 implementation of the same 200-step Cox–Ross–Rubinstein tree. The values are synthetic per-unit option prices. No service or scheduler calls the tool.

Build the CPU baseline at the source revision under test and use a local virtual environment with a compatible OpenCL ICD. The tested Python environment used Python 3.14.4, NumPy 2.5.5 and PyOpenCL 2026.1.5. From this directory:

```sh
go build -trimpath -buildvcs=true -o "$TMPDIR/option-price-cpu" .
python -B ../../../rust/crates/option_pricing/reference/american_reference.py --check
python -B compare.py --cpu-worker "$TMPDIR/option-price-cpu" --device '<exact FP64 GPU name>' > result.json
```

On Windows, pass the path to the built `.exe`. Keep output outside the checkout. The worker executable, application sources, oracle, shuffled input and source revision are recorded by digest or Git identity. Retain `go version -m` for the CPU binary alongside the record to establish its build source and toolchain. Device identity, memory, driver, OpenCL and Python library versions are recorded by the GPU process.

Each of the 100 samples starts one process per backend, alternating backend order. Both workers consume the same seed-1546 permutation of the eight contracts twice. First-batch latency includes process startup, runtime initialization, OpenCL program loading or compilation, JSON transfer, calculation, return transfer and parent-side numerical validation. The second batch measures a warm process. The platform's normal compiler and OS file caches persist between processes. OpenCL event durations for uploads, kernel and downloads are additional device-side observations. Nearest-rank p99 is calculated from complete batches. The experiment covers prices only.

The per-price tolerance in quote units is the existing American oracle budget of `8192 × binary64 epsilon × max(1, |spot|, |strike|)`. Every finite price must pass this budget and each backend must return bit-identical prices on every repetition. The latency criterion for this specific local workload requires both cold and warm p99 below the corresponding CPU p99 over at least 100 samples, with zero additional hardware spend. A short `--samples 1` run checks setup. The comparison stops on worker failure, timeout or an incomplete, nonfinite, nondeterministic or out-of-tolerance response. Its error record retains the inputs and source identities with a nonfinal status. No partial batch is published.

This local comparison does not determine fleet sizing, paid compute cost, eventual GPU dispatch or model eligibility. Those need an approved workload, data and model authority and the capacity/isolation proof in #1545.
