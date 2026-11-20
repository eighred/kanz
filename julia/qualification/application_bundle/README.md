# Compiled calibration application qualification

This source-only profile builds the existing Julia curve worker as a relocatable application using the upstream PackageCompiler API. The compiler, package manager and registry live in the builder. The executable reads one bounded canonical Protobuf request on stdin and writes its response on stdout. Go remains the numerical acceptance and promotion owner.

Use the Julia 1.13.3 compiler with the canonical locked worker depot, an ordinary C compiler, Python 3.11 or later and Git. Prestage the four package archives named in [tool inputs](tool_inputs.json) and a General registry archive named `General.tar.gz` from [commit 0f0f2984](https://github.com/JuliaRegistries/General/archive/0f0f298446cd34ff4f4228983bd8eeabe1f0a83e.tar.gz). Run public acquisition in a separate credential-free process with Python, GnuPG, Git and ordinary HTTPS. It verifies the recorded Julia catalogs and JLL assets, the PCRE2 publisher's detached source signature, its 8-bit Yggdrasil build recipe and the fresh vendor security policy/advisory ranges.

```sh
python3 -B julia/qualification/application_bundle/acquire.py /inputs/native-owners
```

Run the builder on a fresh private writable filesystem with no network, no production credentials and the repository source mounted read-only. Copy the compiler into a new path using the verified PCRE2 10.49 replacement before invoking Julia. This preserves the original Julia tree. The builder checks the real native PCRE2 path and SHA-256 before compiling the application.

```sh
python3 -B julia/qualification/application_bundle/pcre2.py \
  /inputs/native-owners/PCRE2-10.49 /opt/julia /scratch/julia
export PATH="/scratch/julia/bin:$PATH"
python3 -B julia/qualification/application_bundle/stage.py /inputs /scratch/registered
export JULIA_PKG_OFFLINE=true JULIA_PKG_SERVER=''
export JULIA_DEPOT_PATH=/scratch/depot:/scratch/registered/depot:/opt/julia-depot
export JULIA_LOAD_PATH=/scratch/registered/tools:@stdlib
export JULIA_NUM_THREADS=1 JULIA_IMAGE_THREADS=1 OPENBLAS_NUM_THREADS=1
julia --startup-file=no --history-file=no julia/qualification/application_bundle/build.jl \
  "$PWD" /scratch/source /scratch/application
tar -cf /out/bundle.tar -C /scratch/application .
```

The builder binds each registered package's UUID, version, Git tree, archive size and SHA-256. Offline resolution adds the application module and requires all seven original manifest entries to remain identical. The canonical schema generator and numerical tests run before `create_app`. The build uses `filter_stdlibs=true` with `incremental=false` and the generic CPU target. The resolved application manifest is shipped at `/opt/kanz/share/kanz/Manifest.toml` for inventory. [PackageCompiler applications](https://julialang.github.io/PackageCompiler.jl/stable/apps.html)

The local OCI recipe uses the upstream glibc-only distroless Debian 12 base. Verify its pinned digest using Cosign with issuer `https://accounts.google.com` and identity `keyless@distroless.iam.gserviceaccount.com` before acquiring its linux/amd64 platform. Build only from the produced tar. Run the Go conformance suite with the repository's normal schema SDK prepared in the sibling `schemas/gen/go` directory.

```sh
docker build --platform linux/amd64 --network none --iidfile /out/image-id \
  --label "org.opencontainers.image.revision=$(git rev-parse HEAD)" \
  -f julia/qualification/application_bundle/Dockerfile /out
python3 -B julia/qualification/application_bundle/corpus.py "$(cat /out/image-id)" "$(git rev-parse HEAD)" /proof/runtime.json
```

Run a trusted Linux parent with GNU readelf against a Docker 29 OCI export of the selected local index. Save both raw signed-base index/platform manifests, the compiler-produced bundle tar and the SHA-256-pinned official Julia tar. Use the same immutable [native owner inputs](owner_inputs.json) prepared above. The reader verifies the original release catalogs, the signed replacement and each real ELF member independently.

```sh
docker save --output /proof/candidate.oci.tar "$(cat /out/image-id)"
python3 -B julia/qualification/application_bundle/readback.py \
  /proof/candidate.oci.tar /inputs/base-index.json /inputs/base-platform.json \
  /out/bundle.tar /inputs/julia.tar.gz /inputs/native-owners /proof/readback.json "$(cat /out/image-id)"
```

With the Go module cache and schema SDK prepared, run the existing bounded Go process owner in the same credential-free container. Only its cross-compiled static test executable is mounted read-only. It checks the actual application launcher against the OCI readback, sends all thirteen oracle jobs through the existing Go subprocess implementation, checks the deadline and verifies child reaping and scratch cleanup.

```sh
python3 -B julia/qualification/application_bundle/supervise.py /proof/readback.json /proof/supervisor.json
```

The risk-engine interpreter contract requires an absolute executable, project, script and depot. Its supervisor delegates kernel isolation to the deployment. A production compiled-executable transport needs a verified worker identity and an independently enforced network, mount and resource boundary. The qualification parent runs inside that boundary.

The reader compares every application file and symbolic link with compiler output. It compares DT_NEEDED names with the SONAMEs provided by the application and each verified base layer, binds four unmodified Julia core files to the official distribution and eighteen shared libraries to eleven native publishers. Seventeen match the original Julia distribution, and the packaged PCRE2 library matches the separately signed-source 10.49 build selected before compilation. The launcher and application sysimage are generated outputs. The public CA data file copied by PackageCompiler is separately bound to the Julia distribution and the MozillaCACerts_jll archive. The runtime control queries named symbols and uses dladdr to establish their actual owner. Julia core contains utf8proc 2.11.3 with Unicode 17.0.0 and its libuv fork reporting 2.0.0-dev. The standalone libuv library returns the same runtime version. The Julia release links both components into its core and records its selected upstream commits. [Link recipe](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/src/Makefile), [utf8proc pin](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/utf8proc.version) and [libuv pin](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/libuv.version). The offline scanner compares the observed symbol owners with the two exact ELF digests in the saved image. Raw Git commit objects connect Julia's selected sources to utf8proc's published #323 fix and six libuv fix commits for CVE-2014-9748, CVE-2015-0278 and CVE-2024-24806. Each path also reaches a pre-fix control. Acquisition checks current publisher advisories, CVE records and NVD aliases within 24 hours. The scanner rejects a changed denominator, changed applicability or missing graph. The remaining native libraries need their own source and advisory coverage.

Use the repository's checksum-pinned Trivy and Syft archives with the executable and its `.tar.gz` side by side. Stage the complete current read-only Trivy DB under `/cache/db`, the pinned General registry index used by the standard JLL qualifier and an empty writable parent for the results. Run the scanner offline with no credentials, a bounded temporary filesystem and the final OCI export mounted read-only.

```sh
SYFT_CHECK_FOR_APP_UPDATE=false python3 -B julia/qualification/application_bundle/scan.py \
  /proof/candidate.oci.tar /proof/readback.json /proof/runtime.json /inputs/tools /cache \
  /inputs/General.Registry.toml /inputs/native-owners/PCRE2-10.49/publisher.json /out/new-scan
python3 -B -m unittest discover -s julia/qualification -p test_application_bundle.py
```

This second check binds the actual image config and manifest, the seven canonical package UUIDs in both Trivy and SPDX, eleven archive-derived native wrapper records, the packaged CA data owner, the classified Julia runtime in Syft and known-vulnerable HTTP and OpenSSL wrapper controls. It revalidates all nine published PCRE2 project advisory records against their actual source response, the current vendor security policy and both observed native versions. The vendor ranges identify seven records for the original 10.47 library. The same pass independently validates eight published CVE Program affected ranges and two NVD exact-version CPE responses. It refuses changed, stale or missing controls. The 8-bit upstream 10.49 identity is distinct from the 10.49.1+0 wrapper build. It keeps the unmodified results, refuses any applicable wrapper/package finding or critical OS finding, and reports all OS severities. The standard distro's full 23-owner checks remain active.

The compiler-runtime check runs after both scanners and stores `compiler-owners.json` alongside their raw output. It verifies the 643,900,088-byte LLVM_full parent, extracts its actual shared library and compares it with the saved OCI. The full parent selects Julia's LLVM fork at `9cba7d4b`. The LLVM 20.1.8 release is its ancestor. The ordinary amd64 deposit fixture initializes libLLVM, libatomic, libstdc++ and libgcc_s. The other four GCC libraries are present on disk. The public build log binds the original GCC 15.2.0 compiler shard to its catalog. All seven GCC files in this image carry a GCC 15.2.0 ELF compiler comment. [LLVM_full producer](https://github.com/JuliaBinaryWrappers/LLVM_full_jll.jl/tree/4c99685d71410eb0134fe08adb0f1b331b75eee9), [GCC shard catalog](https://github.com/JuliaPackaging/BinaryBuilderBase.jl/blob/e436f7cf3f3ce545d55141180085e4f855c66a53/Artifacts.toml)

The current candidate fails this final check on two native owners. The GCC 15 release-line fix for CVE-2026-95619 changes `libsupc++/new_opa.cc`. GCC 15.2.0 has the exact pre-fix source blob. Both the original 1.5.5+2 and the later 1.5.7+0 CompilerSupportLibraries publish the same 28,633,168-byte amd64 archive. The real amd64 library exports the aligned allocation operator and floating-point from_chars. Debian records its GCC 15 fixes for CVE-2026-95619 in 15.3.0-5, while forky's same source package is still listed as vulnerable for CVE-2026-115456 and CVE-2026-125709. The maintained GCC 15 branch has named fixes for the aligned allocator, PBDS binary heap, parallel search, vector<bool> construction, bitset extraction and PR127666 for from_chars. For the file-stream case, upstream PR118099 changed the implementation on main. The three production changes from upstream PR118099 apply cleanly in order to GCC 15 source at `a311d911`. GCC 16 has a separate backport. The current 15 package still distributes the original ELF. Applying source patches alone does not supply a built library or recompile header consumers. Rebuilding the runtime requires reviewed branch-specific sources and ABI verification for all seven shipped libraries. Other reported libstdc++ cases include header implementations and Red Hat package ranges that need independent applicability decisions. [GCC 15 fix](https://github.com/gcc-mirror/gcc/commit/638de129df3a22917ff2eda13bd0fd861ca9befc), [Debian security record](https://security-tracker.debian.org/tracker/CVE-2026-95619), [CSL 1.5.7](https://github.com/JuliaBinaryWrappers/CompilerSupportLibraries_jll.jl/releases/tag/CompilerSupportLibraries-v1.5.7%2B0)

The published CNA entries for CVE-2026-13573 and CVE-2026-13574 enumerate LLVM 22.1.0 through 22.1.6. Their descriptions refer to earlier releases without defining the Julia fork's affected range. Both named IR symbols are exported with the JL_LLVM_20.1 symbol version in the actual shipped shared library. The first upstream report remains open. The second upstream fix merged into LLVM in July 2026. The Julia fork lacks that commit, and its two IR source blobs differ from both the fixed and pre-fix upstream blobs. The newer Julia 20.x branch tip `68f92238` contains one CFG debug-location change and retains all three IR source blobs from the packaged fork. A supported LLVM 20 assessment or reviewed equivalent backport remains necessary. The scoped refusal preserves the completed scanner results and cannot grant release authority. [LLVM #199187](https://github.com/llvm/llvm-project/issues/199187), [LLVM fix #208278](https://github.com/llvm/llvm-project/pull/208278), [CVE-2026-13574](https://www.cve.org/CVERecord?id=CVE-2026-13574)

The production release sequence starts with approved native advisory coverage, an independently verified immutable source build and a reviewed base in the Eighred mirror. Its main-only publisher copies the upstream index and reads it back. A separate credential-limited consumer must establish access to the identical bytes before replacing the candidate's upstream base. The release owner then verifies signed application publication, immutable pull, bounded supervision and rollback; activation remains subject to the existing governance. [Mirror producer](../../../.github/workflows/base-image-mirror.yml) and [release issue](https://github.com/eighred/kanz/issues/1536)

### Rebuild the C++ runtime for the application candidate

The optional GCC 15.2.0 build uses the signed GNU release tag, seven producer changes and the three BinaryBuilder bootstrap shards in `gcc_runtime_inputs.json`. Six changes come from the GCC 15 series. The seventh uses the GCC 16 file-stream backport that preserves the original exported C++ signatures. The production-source preparer checks Git blob IDs, the release signer and all seven applied file hashes. Run it with Git, GnuPG, Python 3.11 or later and GNU binutils. Fetch the release and fix objects from [the official GCC mirror](https://github.com/gcc-mirror/gcc), the pinned release archive from its commit, and the [GNU keyring](https://ftp.gnu.org/gnu/gnu-keyring.gpg). Put the three pinned shards in a new bootstrap directory. Run the compiler in a private, network-disabled Linux/amd64 container.

```sh
q=julia/qualification/application_bundle
python3 -B "$q/gcc_runtime.py" prepare "$inputs/gcc152-sources.tar.gz" \
  "$inputs/gcc-release.git" "$inputs/gcc-fixes.git" "$inputs/gnu-release-keys.gpg" "$out/gcc-production.tar"
python3 -B "$q/gcc_runtime.py" verify-shards "$inputs/bootstrap"
docker build --platform linux/amd64 --network none --iidfile "$out/bootstrap.id" \
  -f "$q/gcc_runtime.Dockerfile" "$inputs/bootstrap"
docker run --rm --pull=never --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges --user 65532:65532 --cpus 3 --memory 4g --memory-swap 4g --pids-limit 128 \
  --tmpfs /work:rw,nosuid,exec,size=2200m,mode=1777 --tmpfs /source:rw,nosuid,exec,size=700m,mode=1777 \
  --tmpfs /tmp:rw,nosuid,noexec,size=64m,mode=1777 \
  --mount "type=bind,src=$out/gcc-production.tar,dst=/vendor-release.tar,readonly" \
  --mount "type=bind,src=$PWD/$q/build_gcc_runtime.sh,dst=/qualification.sh,readonly" \
  --mount "type=bind,src=$PWD/$q/gcc_runtime_controls.cc,dst=/cxx-surface.cc,readonly" \
  --mount "type=bind,src=$out/runtime,dst=/out" "$(cat "$out/bootstrap.id")" /qualification.sh
python3 -B "$q/gcc_runtime.py" stage-compiler "$scratch/pcre-compiler" \
  "$out/runtime/libstdc++.so.6.0.34" "$scratch/rebuilt-compiler"
```

Use fresh, absolute input, scratch and output paths. Give UID 65532 write access to the empty runtime output directory. Select the staged compiler for the ordinary `build.jl` invocation above. Provide `--rebuilt-gcc "$out/runtime/libstdc++.so.6.0.34"` to readback. The receipt binds the actual OCI bytes and the original GCC JLL publisher bytes separately. All 6,097 versioned exports and their types, the SONAME, the dynamic dependencies and the glibc bounds are checked against the original. The other six shipped GCC libraries retain their publisher hashes. Run the benign standard-library controls from the build on the resulting immutable application image with the application's own library search path.

Rebuilding the C++ shared library addresses the compiled aligned allocation path. Five header changes still require provenance for the prebuilt consumer binaries. The from_chars and file-stream changes correspond to GCC PR127666 and PR118099. The CentOS Stream GCC 15 package spec maps CVE-2026-115456 to PR127666. Red Hat's Hummingbird patch header maps CVE-2026-125709 to the GCC 16 backport of PR118099. The offline qualifier verifies the two immutable specs, the selected patch metadata and both compiled source identities. The independent LLVM review and all release checks still apply.

### Rebuild Julia's private runtime for the safe AOT application

The Linux/amd64 producer uses the signed Julia 1.13.3 source archive and the public input identities in `core_runtime_inputs.json`. Put its named files in one empty input directory. Extract `./opt/kanz-gcc/lib64/libstdc++.a` from the preceding GCC producer's `installed-libstdcxx.tar.gz`. Put the same musl root and GNU platform shards and the verified GCC 9.1 target shard in a fresh bootstrap directory. Name the target shard `gcc-gnu-bootstrap.tar.gz`. The official compiler image runs the Julia build as UID 65532 and uses the GNU sysroot's loader for the publisher's llvm-config. [Julia release](https://github.com/JuliaLang/julia/releases/tag/v1.13.3) and [LLVM_full producer](https://github.com/JuliaBinaryWrappers/LLVM_full_jll.jl/tree/4c99685d71410eb0134fe08adb0f1b331b75eee9)

```sh
q=julia/qualification/application_bundle
python3 -B "$q/core_runtime.py" collect-vector-patch "$inputs/gcc8-vector.production.diff"
python3 -B "$q/core_runtime.py" verify-shards "$bootstrap"
python3 -B "$q/core_runtime.py" prepare "$inputs" "$gcc_fixes_git_dir" "$out/prepared"
docker build --platform linux/amd64 --network none --iidfile "$out/core-bootstrap.id" \
  -f "$q/core_runtime.Dockerfile" "$bootstrap"
docker run --rm --pull=never --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
  --user 65532:65532 --cpus 2 --memory 4g --memory-swap 4g --pids-limit 128 \
  --tmpfs /tmp:rw,nosuid,exec,size=2500m,mode=1777 \
  --mount "type=bind,src=$out/prepared,dst=/input,readonly" \
  --mount "type=bind,src=$out/prepared/headers/libstdc++-v3/include,dst=/headers,readonly" \
  --mount "type=bind,src=$PWD/$q/build_core_runtime.sh,dst=/core.sh,readonly" \
  --mount "type=bind,src=$out/build,dst=/out" "$(cat "$out/core-bootstrap.id")" /core.sh
python3 -B "$q/core_runtime.py" verify-abi "$original_core" "$out/build/libjulia-internal.so.1.13.3" \
  "$extracted_base_libc" "$extracted_application_libstdcxx" "$out/build/libjulia-internal.linkmap"
```

Use an empty build output directory writable by UID 65532. The preparer verifies Julia's release signature, four GCC 9.1 header preimages and postimages, the five utf8proc source blobs and all archive hashes. The producer rebuilds utf8proc, flisp, support, the generated boot image and Julia's canonical release target. The original GCC 9 bitset predates the changed extractor. The checked link map identifies the corrected static C++ allocator and 50 prebuilt LLVM objects from Support, TargetParser and Demangle. The publisher compiled those objects with GCC 10.2.0. Their absence of debug-line metadata limits statements about optimized transitive header uses. No LLVM IR object is selected.

The ELF check compares all 1,698 exported names and types, the 11 direct dependencies, SONAME and RUNPATH. The new private DSO requires `secure_getenv@GLIBC_2.17`. The pinned base provides that symbol, and the same application already requires it in libstdc++. This check covers the pinned amd64 image. Rebuild a fresh safe bundle with this core, compare its full OCI file inventory, and run the direct corpus, bounded Go supervisor and actual-image advisory checks before assessing a release.

### Identify the LLVM objects linked into Julia

The Julia link map selects 50 members from the LLVM_full release. Its build log identifies the exact GCC 10.2 target tree `eaafa83c8c4758dc2d5a6b28b145e59d6d751626` and GNU platform tree `0a4fcd057d87f56cd0a41a9e860a8a3ee80498a2`. Use the root and platform shards above with the [GCC 10.2.0 target shard](https://github.com/JuliaPackaging/Yggdrasil/releases/download/GCCBootstrap-v10.2.0%2B0/GCCBootstrap-x86_64-linux-gnu.v10.2.0.x86_64-linux-musl.unpacked.tar.gz) under the bootstrap target filename. The preparer verifies all three, replays the Red Hat production delta against the actual GCC 10.2 header and stages the selected LLVM production sources. The CMake inputs come from the same LLVM_full archive and its [pinned source fork](https://github.com/JuliaLang/llvm-project/tree/9cba7d4b7efbbc8d3755144e3deb741de8011b41).

```sh
python3 -B "$q/llvm_core_inputs.py" collect "$out/llvm-sources"
python3 -B "$q/llvm_core_inputs.py" prepare "$out/llvm-sources" "$inputs/full-parent.tar.gz" \
  "$gcc10_bootstrap" "$inputs/gcc8-vector.production.diff" "$out/llvm-inputs"
docker build --platform linux/amd64 --network none --iidfile "$out/llvm-bootstrap.id" \
  -f "$q/core_runtime.Dockerfile" "$gcc10_bootstrap"
```

Use the same unprivileged, read-only and network-disabled container settings as the core build above. Run `configure_llvm_core.sh` with the new `source.tar` at `/source.tar`, the Zlib and Zstd wrapper archives at `/zlib.tar.gz` and `/zstd.tar.gz`, and an empty writable `/out`. The script checks both generated header hashes and preserves CMake's actual exit code. The restricted source selection contains the inputs to the header producer. Whole-project configuration exits 1 when it subsequently reaches other target directories. The two generated headers and the complete configuration log are retained. The public header is byte-identical to the publisher copy. A whole LLVM build requires the full producer source.

Run `python3 -B /verify/llvm_core_objects.py` inside the same immutable compiler with three CPUs, 2.8 GB of memory, a 650 MB executable temporary filesystem and these mounts. Mount all inputs read-only.

| Container path | Host input |
| --- | --- |
| `/verify` | This qualification directory |
| `/original-inputs.tar` | The core producer's `prepared/native-relink-inputs.tar` |
| `/source.tar` | The LLVM preparer's `source.tar` |
| `/fixed` | The LLVM preparer's `headers/libstdc++-v3/include` |
| `/config` | The saved generated header output |
| `/out` | Fresh writable results directory for UID 65532 |

The compiler determines the real source dependencies and builds each member twice with GCC 10.2, before and after correcting its vector header. All 50 independently rebuilt object pairs must be byte-identical. For 49 members the corrected rebuild matches the entire published object. The 50th member, Triple.cpp, differs in two bytes belonging to the private switch table's section and symbol names. Both complete object hashes, the two label digits and all remaining bytes are checked. The switch table has one local symbol and 40 data bytes. All 2,758 executable sections across the 50 objects match. This comparison covers the five named GCC header cases in these linked LLVM members. New advisories, another member selection or a different published compiler require their own assessment.
