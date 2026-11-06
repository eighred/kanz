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

Syft must identify the actual Julia 1.13.3 binary in that same image and provides
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

Syft's pinned stereoscope provider reports a **config digest**, which is not
necessarily Docker29's manifest/index image ID. The qualification creates a
unique local alias only after successful absence readback, verifies its Docker
ID equals the qualified image ID, then reuses the existing strict Docker-save
single-manifest/config parser from tv-sync's isolation harness. The exported
content-addressed config bytes must hash correctly and describe linux/amd64;
Syft must report that exact config digest. The alias is checked again after
export. This is not a label-only or cross-kind digest comparison. Only validated
IDs/version/type are projected for diagnostics; config contents remain private.

The runtime image excludes five complete upstream development directories:
`share/julia/base/JuliaSyntax/docs`, `share/julia/stdlib/v1.13/TOML/docs`,
`share/julia/stdlib/v1.13/TOML/benchmark`, `share/julia/stdlib/v1.13/Tar/test`,
and `share/julia/test`. Their bundled documentation, benchmark and deliberately
invalid test environments are not the worker environment. The pinned archive's
runtime source, libraries, standard libraries and license remain; the worker's
own goldens remain in the qualification image. The actual image probe verifies
these directories are absent and records the exact set. This changes packaged
content rather than suppressing scanner results: the whole image still passes
through the same scan, and any remaining CRITICAL advisory refuses qualification.
The failed pre-removal scan is retained in PR #1472; its development manifests
do not by themselves establish that those historical native binaries were loaded.

The stacked native inventory qualification independently hashes the same verified
Julia archive retained by the host setup, without extracting its members. The
bounded, read-only image probe inventories all regular files and relative links
under `/opt/julia/{bin,lib,libexec,share/julia/compiled}` and the bundled JLL Project UUID/version and
raw metadata bytes. Exact absence/addition/hash/link differences refuse. Loaded
depot package images are separately inventoried as generated, unqualified bytes.
Loaded-library closure requires each observed library to belong to the verified
archive, generated depot image inventory, or separately recorded system paths.
This loaded set is observed in the qualification probe, not every future worker
`dlopen` path. System libraries outside the Julia archive are listed with bounded hashes
under system library paths; these observations do not assign Debian package
ownership. The existing Syft Debian inventory remains distinct.

The archive inventory conservatively retains every selected runtime file as
unassigned; a separate, narrowly qualified mapping below binds one component. Exact archive equality proves provenance, not vulnerability
clearance or reachability. Julia sysimages, bundled patched libraries and system
libraries need component-specific advisory mappings and known-vulnerable controls;
the native/runtime release blocker is unchanged. No package is imported just to
query its version and no native library is executed by the host inventory parser.

The advisory lane also derives a **scanner-only** Project/Manifest pair from the 23 JLL
UUID/version records already matched against the actual image and the pinned
Julia archive. It verifies each UUID/name/path against checksum-pinned
[General registry metadata](https://github.com/JuliaRegistries/General/blob/afacc2e68d6c156d80eb9ff489370bb38d1a61f4/Registry.toml)
before using Trivy's Julia analyzer. The Project names all 23 UUIDs as direct
scanner inputs because pinned Trivy resolves Manifest reachability from Project
dependencies. This does not instantiate packages or
change the worker's Project/Manifest. The same fresh advisory database must
identify the manifest-only OpenSSL_jll 3.0.12+0 control for
[JLSEC-2026-252 / CVE-2024-5535](https://github.com/JuliaLang/SecurityAdvisories.jl/blob/main/advisories/published/2026/JLSEC-2026-252.md).
Every raw finding for the actual JLL metadata remains in the report. A finding
refuses qualification unless the separate exact native identity mapping below
proves that specific installed component is outside the reviewed affected range. Evidence binds the registry commit/checksum, all 23
UUID/versions, both scanner-input checksums and control outcome to the existing
image/source/database report.

This is partial metadata coverage, not a claim that every native file is owned
by a General package or that Julia's patched libraries match upstream release
semantics. Generated native images, system libraries, Julia runtime code and
unassigned files retain their explicit ownership/coverage limitations. The
[Julia advisory database scope](https://github.com/JuliaLang/SecurityAdvisories.jl#scope)
is incomplete for artifacts. Standard release publication remains blocked on
that unresolved coverage; no scanner suppression or native clearance is added.

The first qualified native identity is the archive's `libexec/julia/7z`. Julia
1.13.3 bundles 7-Zip **26.03** while its compatibility `p7zip_jll` metadata says
17.8.3+0. The unchanged raw Trivy finding for CVE-2026-58052 is retained, not
suppressed. Qualification requires the exact independently archive-bound binary
hash, its successful `7z i` banner executed inside the same restricted OCI image,
checksum-pinned [Julia build metadata](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/p7zip.version)
and [upstream release history](https://github.com/ip7z/7zip/blob/0766b733fe3e06dd2a7f9a3cfbf2108ac73abd17/DOC/src-history.txt),
and a newly retrieved, analyzed [NVD record](https://nvd.nist.gov/vuln/detail/CVE-2026-58052).
The exact CPE/range must identify versions before 26.03 as affected; numerical
controls must classify 26.02 as affected and 26.03 as fixed. These controls use
version inputs, not execution of a vulnerable binary. Public source bytes and
the bounded NVD response/checksum are retained for independent validation.
Freshness is evaluated at the recorded scan-time qualification window; an old
passing artifact is not permanent advisory clearance. HTTPS redirects cannot
downgrade to HTTP.
Changed ranges, additional CPE conditions, unknown findings, mismatched UUIDs,
wrapper/native versions, hashes, source bytes or stale advisory snapshots refuse.
No file or worker dependency is removed. This corrects one component identity;
it does not clear other native/runtime files, platform reachability, or future
advisories. Standard release publication remains blocked on residual UNKNOWN
coverage under #1413.

The OCI runtime explicitly replaces Julia 1.13.3's bundled OpenSSL 3.5.8
`libcrypto.so.3` and `libssl.so.3` with the two regular library members from the
checksum-pinned official [OpenSSL 3.5.9 Julia artifact](https://github.com/JuliaBinaryWrappers/OpenSSL_jll.jl/blob/4d127b8384f6a14b3963070f4677730e94407fc5/Artifacts.toml).
The original `OpenSSL_jll` 3.5.8+0 metadata and worker dependency files remain
unchanged; evidence distinguishes that metadata from the repaired native bytes.
This is a two-library runtime overlay, not an assertion that upstream Julia 1.13.3
already contains the fix. The upstream license is retained. Independent archive
inventory must match every other selected Julia file exactly and these two files
must match the independently verified replacement archive. The restricted image
executes `OpenSSL_version(0)` and initializes/frees an SSL context from that exact
library pair. Neither operation opens a network connection.

Qualification also retrieves the complete official
[OpenSSL CNA catalog](https://openssl-library.org/news/secjson/), binds its index
and every indexed record by hash, and evaluates all recognized affected ranges.
Unknown shapes, missing records, any affected installed version, or failure to
detect the 3.5.8/CVE-2026-84782 control refuse qualification. The control uses
version-range inputs, not a vulnerable executable. The initial complete catalog
identified 13 advisories affecting 3.5.8, including the HIGH
[DTLS advisory](https://openssl-library.org/news/secadv/20260929.txt); this does
not establish a reachable exploit in the network-disabled calibration worker.
The fresh catalog and executed repaired image remain mandatory. This additional
component qualification does not resolve the residual native/runtime coverage
blocker or authorize standard release publication.
