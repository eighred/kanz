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
julia --startup-file=no --history-file=no --project=julia -e 'using Pkg; Pkg.instantiate()'
julia --startup-file=no --history-file=no --project=julia julia/generate.jl
julia --startup-file=no --history-file=no --project=julia julia/test/runtests.jl
```

The runtime command is `julia --startup-file=no --history-file=no
--compiled-modules=existing --pkgimages=existing --project=<absolute-julia>
<absolute-julia>/bin/curve_worker.jl`. It reads one
raw protobuf request through stdin EOF and writes one raw protobuf result before
exit, each at most 1 MiB. Runtime does not instantiate packages, download data,
read credentials or open network connections. The supervisor must supply its
prebuilt dependency depot and scrubbed environment. The existing supervisor
bounds pipes and process duration. Its runtime/project/depot are trusted code;
it is not an OS or network sandbox. Operational network/resource isolation is
still required before deployment. Package loading reads the installed runtime
and generated source files.

## Offline source-package input

`qualification/offline_depot.py` verifies the three external source packages in
`qualification/offline_packages.json` against the canonical LF bytes of the locked
Manifest. The trusted parent supplies a read-only input directory containing
`Manifest.toml`, `offline_packages.json` and one `<Package>.tar.gz` per declared
package. It separately creates and retains a private empty scratch depot.
The preparer takes these directories as `--input` and `--depot`; invoke it only
inside the reviewed Linux sandbox with no network, an empty allowlisted
environment, read-only inputs/tools and explicit resource/deadline limits.

Preparation performs no downloads, package installation, Julia execution or
precompilation. It checks compressed SHA256/size, the complete archive census,
upstream Git tree and Project UUID/version before descriptor-relative exclusive
writes. Links, special files, ambiguous paths, build scripts, artifact manifests,
alternative source entries and unexpected inputs refuse. All package validation
precedes output creation. Output files are read-only and `consumerAccepted` is
always false.

After preparation, run the consumer check against the private depot in the same
isolated Linux environment. Use the selected Julia 1.13.3 executable. Keep the
depot writable for this step so the package loader can create its caches.

```sh
JULIA_DEPOT_PATH="$depot" JULIA_LOAD_PATH='@:@stdlib' JULIA_PKG_OFFLINE=true \
  julia --startup-file=no --history-file=no --project=julia julia/test/offline_depot.jl
```

The check uses Julia's package slug algorithm and the locked manifest to locate
each external package. It verifies the source path and version actually loaded
by the worker's ProtoBuf import and the presence of its compiled package caches.
For numerical and wire behavior, run the canonical generation and test commands
above with the same prepared depot. Image adoption, native advisory qualification
and deployment acceptance remain tracked in #1536.

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
python3 -B -m unittest discover -s julia/qualification -p 'test_*.py'
cd kanz
go test -p 1 ./tools/julia_conformance -count=1
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
The same check reconciles a complete, recent NVD source page for OpenSSL's CNA
with this directory. The [CVE Program](https://www.cve.org/Downloads) supplies
the assigning CNA, published/rejected state and affected ranges for every
ID on that source page. Five known discovery controls must remain accounted
for, including after an ID moves into the vendor directory. Confirmed
rejections require agreement between the CVE Program and NVD and absence
from the published vendor directory. Overlapping records must agree on their
affected products and ranges. Forty-five historical CVE Program records from
20162022 express OpenSSL 1.0, 1.1 or 3.0 releases in prose. For each, the
[reviewed digest](qualification/openssl_historical_ranges.json) binds both
authorities' exact affected projections to that CVE before evaluating the
vendor's machine-readable ranges. A revision to either projection requires
agreement or a new review. The receipt retains the source page and every
discovered CVE Program record with SHA-256 digests. Incomplete discovery,
conflicting publishers or states, unknown ranges and unavailable sources
refuse qualification.
Unknown shapes, missing records, any affected installed version, or failure to
detect the 3.5.8/CVE-2026-84782 control refuse qualification. The control uses
version-range inputs, not a vulnerable executable. The initial complete catalog
identified 13 advisories affecting 3.5.8, including the HIGH
[DTLS advisory](https://openssl-library.org/news/secadv/20260929.txt); this does
not establish a reachable exploit in the network-disabled calibration worker.
The fresh catalog and executed repaired image remain mandatory. This additional
component qualification does not resolve the residual native/runtime coverage
blocker or authorize standard release publication.

The subsequent native repair replaces the original `libcurl.so.4.8.0` bytes
(libcurl 8.22.0) with the same ABI member from the pinned official
[LibCURL 8.23.0 artifact](https://github.com/JuliaBinaryWrappers/LibCURL_jll.jl/blob/c268bc7d6fdaa4ae6514fe42d81e0b7710887b86/Artifacts.toml).
Its license and independent artifact/member hashes are required; original
`LibCURL_jll` 8.22.0+0 metadata remains visible. The source build associated with
the original artifact applies no 8.22 patch and enables HTTP/2/OpenSSL, making
[CVE-2026-92392](https://curl.se/docs/CVE-2026-92392.html) a known component match.
This does not establish an exploit path in the network-disabled worker.

The restricted image executes `curl_version_info`, verifies 8.23.0, the linked
OpenSSL 3.5.9 backend and TLS/HTTP2 feature bits, records bounded protocols, and
initializes/frees an easy handle without a network request. Fresh complete
official [8.22.0](https://curl.se/docs/vuln-8.22.0.json) and
[8.23.0](https://curl.se/docs/vuln-8.23.0.json) version-specific advisory arrays
must parse successfully. The former must identify the HIGH control; any record
for the installed version refuses. Empty output, HTTP failure, mismatched
affected-version lists/ranges and unknown response shapes cannot stand in for
an empty advisory array. Public responses/hashes and scan-time qualification
are retained; this is component coverage, not permanent or general clearance.

`qualification/install-native-runtime.sh` is the single fixed recipe used by
the OCI build and the disposable Linux CI Julia installation **before** Pkg
network access. It verifies the original runtime and all three archives, checks
all original library members before replacement, and refuses redirected roots,
linked targets, mixed installations or repeat application. Original dependency
metadata stays unchanged. The Windows runtime is never patched. Linux unit
qualification executes the recipe on an independently archive-derived temporary
tree and tests prewrite tamper/symlink refusal; those tests require the three
verified archives in `RUNNER_TEMP`. Partial installer failure stops the build;
this is not a live installation or transactional rollback mechanism.

### Development-source candidate

The separate candidate accepts the unsigned OpenSSL development commit `65e1164883045c787cf2c25b486f7424eef5aee7` for a disposable offline build. The production Dockerfile and its qualification still require the published 3.5.9 artifact. Run from a clean LF checkout with the usual independently generated Julia and Go schema bindings. Use a dedicated public-input directory with the verified Julia and libcurl archives named `julia.tar.gz` and `libcurl.tar.gz`. Its `packages/` directory holds the three pinned source archives, the committed `Manifest.toml` and `qualification/offline_packages.json`. The [producer artifact](https://github.com/eighred/kanz/actions/runs/44185887107/artifacts/14376625018) must be the original ZIP.

```sh
python3 -B julia/qualification/source_candidate.py stage "$PRODUCER_ZIP" "$PUBLIC_INPUTS/openssl.tar.gz"
python3 -B julia/qualification/source_candidate.py verify "$PUBLIC_INPUTS" "$PRODUCER_ZIP" \
  "$UPSTREAM_EVIDENCE" "$NEW_EVIDENCE_DIRECTORY"
```

Use the Docker containerd image store that exports an OCI index and returns its immutable descriptor ID. A classic config-ID-only Docker archive refuses. The upstream evidence directory contains the original GitHub git/commits response as `upstream.json` and the eight `CVE-*.compare.json` responses from the exact producer URLs. Preserve original bytes. The trusted parent builds the committed Git archive, saves its OCI, checks the manifest/config/layer digests and independently hashes the three ELF members in the compressed layers. The checker verifies the full native inventory, original JLL metadata, local 7zip identity, installed source packages and generated contracts. It uses the ordinary no-network worker checks, numerical assertions, 13 Go corpus cases and timeout cleanup. The combined vendor-directory and CVE Program records must match the reviewed record set. The eight-fix source input lacks coverage for CVEs now published by the OpenSSL CNA; its stored passing result is historical. Every advisory on the 3.5.9 baseline must carry the same upstream fix and recorded ancestry as the two independently reproduced outputs. A changed record, missing control, stale observation or changed installed byte refuses. The receipt always retains `releaseQualified=false` and `consumerAccepted=false`. Wider native advisory ownership, upstream 7zip mapping, signed release, rollout and rollback retain their separate acceptance gates.
