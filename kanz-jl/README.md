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

## Source-only OCI qualification

`Dockerfile` packages this same worker and its generated contracts using the
existing pinned private build/runtime mirrors and the checksum-verified Julia
1.13.3 Linux archive. The runtime is distroless and runs as UID/GID65532. It does
not expose a server. The existing Go supervisor still invokes its configured
local Julia executable; it does not invoke Docker or accept an image reference.
No active deployment or standard build/release matrix uses this image yet.

The `julia-calibration` CI job builds the image with the exact source SHA, then
runs `qualification/proof.py`. Each invocation uses the actual image ID with
network disabled, a read-only root/depot, dropped capabilities, no-new-privileges,
one CPU,1GiB memory/swap limit,64PIDs and a16MiB noexec temporary filesystem.
There are no host bind mounts or application credentials. In-image assertions
check the effective UID, capabilities, cgroup limits, absence of non-loopback
interfaces, and refusal of writes to the installed project/depot/root. Existing
independent rational goldens run in the same final image. A test-only generated
Go protobuf oracle then exercises the unmodified entrypoint with13 success and
refusal inputs, including missing-versus-zero rates and malformed/oversized wire.
Pipe sizes and execution deadlines are bounded; owned containers are removed
and their absence verified before success.

Evidence binds the source SHA and image ID to the runtime archive checksum,
actual installed package Git-tree hashes, locked Project/Manifest hashes,
worker/test source hashes and independently generated canonical contract bytes.
The numerical tolerance remains the existing1e-14/1e-13 discount-factor bound.
No tolerance is widened for container execution. The artifact contains only
public source/dependency identities and fixed fixture outcomes, not environment
variables, application configuration, request contents or raw process errors.

**Release blocker (#1413):** this is package identity evidence, not Julia
dependency vulnerability clearance. Syft can identify the Julia binary, but
Julia package advisory coverage has not been qualified. This slice neither
claims vulnerability absence nor adds signing, registry publication or a
production transport. Those require a separately reviewed release policy and
deployment integration. A passing disposable image test is not production
curve authority, a hardened multi-tenant compute service, or live acceptance.

Local logic checks (actual image execution requires Linux/Docker):

```sh
python3 -B -m unittest discover -s kanz-jl/qualification -p 'test_*.py'
cd kanz
go test -p 1 ./tools/julia-conformance -count=1
go test -p 1 ./test/arch -run '^TestJulia' -count=1
```

The unconfigured OCI oracle skips in ordinary Go runs; the dedicated workflow
sets its fixture directory/phase and rejects every skipped or missing outcome.

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

### Actual-image advisory qualification (source only)

The additive `qualification/advisory.py` step consumes the successful OCI
qualification's exact local image ID; it neither publishes nor executes another
worker image. It verifies the reviewed Trivy0.72.0 and Syft1.56.0 release archive
checksums, records executable hashes, downloads a fresh advisory database and
records its SHA256 and update/download timestamps. The same database snapshot is
used throughout the bounded scan; a stale/missing/changed database refuses.

Trivy scans the actual image. Its Julia report and generated SPDX identities must
contain all seven locked UUID/version pairs, including the three external
packages whose installed Git trees were independently checked in the isolated
image. Those three names are additionally bound to their reviewed General
registry UUIDs. Missing/duplicate/substituted inventory, suppressed findings or
any known advisory in the worker's Julia dependency closure refuses. Critical
findings elsewhere in the actual image also refuse; lower-severity findings are
retained as public projected evidence, never described as absent. A separate
manifest-only HTTP1.10.15 fixture must detect CVE-2025-52479/JLSEC-2025-1 using the
same database. No vulnerable package is installed or executed.

Syft must identify the actual Julia1.13.3 binary in that same image and provides
runtime/OS package inventory. Raw reports can contain image configuration, so
they remain in a temporary directory; the artifact contains only projected public
package/advisory/tool/database identities. Scanner subprocesses inherit no GitHub
or application credentials. Existing images and shared caches remain untouched; only the new private scanner
scratch directory is removed.

This is partial advisory qualification, **not release clearance**. Trivy's Julia
source supports Manifest/Project dependency graphs and advisory lookup, while
Syft1.56's Julia support is a binary classifier, not a Julia dependency cataloger.
JLSEC covers General registry packages and explicitly warns that artifact/JLL
advisories are incomplete. Inventory of four bundled stdlibs, the Julia runtime
and native libraries does not establish complete advisory coverage for them.
Those coverage limits remain a release blocker under #1413; the standard release
matrix, signing policy, production transport and Go authority are unchanged.

Primary pinned evidence:
- [Trivy0.72 Julia support](https://github.com/aquasecurity/trivy/blob/v0.72.0/docs/guide/coverage/language/julia.md)
- [Trivy0.72 manifest UUID parser](https://github.com/aquasecurity/trivy/blob/v0.72.0/pkg/dependency/parser/julia/manifest/parse.go)
- [Trivy0.72 Julia advisory driver](https://github.com/aquasecurity/trivy/blob/v0.72.0/pkg/detector/library/driver.go)
- [Syft1.56 binary classifiers](https://github.com/anchore/syft/blob/v1.56.0/syft/pkg/cataloger/binary/classifiers.go)
- [JLSEC scope and limitations](https://github.com/JuliaLang/SecurityAdvisories.jl#scope)
