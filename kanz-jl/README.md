# Julia curve shadow worker

This worker compares an immutable risk-engine calibration capture with an
independent numerical implementation. It returns research candidates only.
The existing Go curve, artifact recording and admission paths retain authority.
No worker output installs a curve or publishes an event.

Julia is pinned to **1.13.3** and ProtoBuf.jl to **1.3.0**. Build-time generation
uses the canonical `research.v1` contract and its imports, never a hand-written
wire model. Generated Julia sources live in ignored `gen/`.
The input rate uses an explicit protobuf `oneof`: ProtoBuf.jl 1.3.0 erases
proto3 optional scalar presence, so optional-double defaults cannot distinguish
an absent quote from a measured zero. Wire tests require both cases to survive.

```sh
julia --startup-file=no --history-file=no --project=kanz-jl -e 'using Pkg; Pkg.instantiate()'
julia --startup-file=no --history-file=no --project=kanz-jl kanz-jl/generate.jl
julia --startup-file=no --history-file=no --project=kanz-jl kanz-jl/test/runtests.jl
```

The runtime command is `julia --startup-file=no --history-file=no
--compiled-modules=existing --pkgimages=existing --project=<absolute-kanz-jl>
<absolute-kanz-jl>/bin/curve_worker.jl`. It reads one
raw protobuf request through stdin EOF and writes one raw protobuf result before
exit, each at most 1 MiB. Runtime does not instantiate packages, download data,
read credentials or open network connections. The supervisor must supply its
prebuilt dependency depot and scrubbed environment. The existing supervisor
bounds pipes and process duration. Its runtime/project/depot are trusted code;
it is not an OS or network sandbox. Operational network/resource isolation is
still required before deployment. Package loading reads the installed runtime
and generated source files.

Supported conventions are linear interpolation in continuous zero rates with
flat extrapolation; simple-interest deposits and forwards; annual swap fixed
coupons with a final stub. Future nonpositive span retains the Go default of
0.25 years. Inputs are analytical Float64 rate fractions and year fractions,
not a new money representation. The worker supports at most 256 inputs and
100-year maturities, rejects nonfinite inputs, duplicate maturities, missing
future anchors, invalid discount factors and unsupported interpolation. Missing
resolved rates are not fabricated; the immutable request retains coverage and
observation provenance, and the Go comparison owns their interpretation.

The solver evaluates the fixed-leg PV equality independently of Go's par-rate
objective. Tests use hand-derived discount factors 20/21 (deposit), 2000/2121
(forward), 330/371 (swap), 20/19 (negative rate) and 50/51 (stub), with absolute
discount-factor tolerances of 1e-14 or 1e-13. Numerical agreement is not approval
to promote the candidate.

Primary API/version references: [Julia downloads](https://julialang.org/downloads/),
[ProtoBuf.jl reference](https://juliaio.github.io/ProtoBuf.jl/dev/reference/),
[ProtoBuf.jl package](https://github.com/JuliaIO/ProtoBuf.jl).
