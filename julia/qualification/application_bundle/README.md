# Compiled calibration application qualification

This source-only profile builds the existing Julia curve worker as a relocatable application using the upstream PackageCompiler API. The compiler, package manager and registry live in the builder. The executable reads one bounded canonical Protobuf request on stdin and writes its response on stdout. Go remains the numerical acceptance and promotion owner.

Use the Julia 1.13.3 compiler with the canonical locked worker depot, an ordinary C compiler, Python 3.11 or later and Git. Prestage the four package archives named in [tool inputs](tool_inputs.json) and a General registry archive named `General.tar.gz` from [commit 0f0f2984](https://github.com/JuliaRegistries/General/archive/0f0f298446cd34ff4f4228983bd8eeabe1f0a83e.tar.gz). Run public acquisition in a separate credential-free process with Python, GnuPG and ordinary HTTPS. It verifies the recorded Julia catalogs and JLL assets, the PCRE2 publisher's detached source signature, its 8-bit Yggdrasil build recipe and the fresh vendor security policy/advisory ranges.

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
python3 -B julia/qualification/application_bundle/corpus.py "$(cat /out/image-id)" "$(git rev-parse HEAD)"
```

Run a trusted Linux parent with GNU readelf against a Docker 29 OCI export of the selected local index. Save both raw signed-base index/platform manifests, the compiler-produced bundle tar and the SHA-256-pinned official Julia tar. Use the same immutable [native owner inputs](owner_inputs.json) prepared above. The reader verifies the original release catalogs, the signed replacement and each real ELF member independently.

```sh
docker save --output /proof/candidate.oci.tar "$(cat /out/image-id)"
python3 -B julia/qualification/application_bundle/readback.py \
  /proof/candidate.oci.tar /inputs/base-index.json /inputs/base-platform.json \
  /out/bundle.tar /inputs/julia.tar.gz /inputs/native-owners /proof/readback.json "$(cat /out/image-id)"
```

The reader compares every application file and symbolic link with compiler output. It compares DT_NEEDED names with the SONAMEs provided by the application and each verified base layer, binds four unmodified Julia core files to the official distribution and eighteen shared libraries to eleven native publishers. Seventeen match the original Julia distribution, and the packaged PCRE2 library matches the separately signed-source 10.49 build selected before compilation. The launcher and application sysimage are generated outputs. The public CA data file copied by PackageCompiler is separately bound to the Julia distribution and the MozillaCACerts_jll archive. The runtime control queries named symbols and uses dladdr to establish their actual owner. Julia core contains utf8proc 2.11.3 with Unicode 17.0.0 and its libuv fork reporting 2.0.0-dev. The standalone libuv library returns the same runtime version. The Julia release links both components into its core and records its selected upstream commits. [Link recipe](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/src/Makefile), [utf8proc pin](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/utf8proc.version) and [libuv pin](https://github.com/JuliaLang/julia/blob/67ac47b9cd855e02134d41d5428ac9f2a73f2694/deps/libuv.version). This provides binary provenance; security advisories for the native projects and statically linked code still require their own supported ranges, controls and policy.

Use the repository's checksum-pinned Trivy and Syft archives with the executable and its `.tar.gz` side by side. Stage the complete current read-only Trivy DB under `/cache/db`, the pinned General registry index used by the standard JLL qualifier and an empty writable parent for the results. Run the scanner offline with no credentials, a bounded temporary filesystem and the final OCI export mounted read-only.

```sh
SYFT_CHECK_FOR_APP_UPDATE=false python3 -B julia/qualification/application_bundle/scan.py \
  /proof/candidate.oci.tar /proof/readback.json /inputs/tools /cache \
  /inputs/General.Registry.toml /inputs/native-owners/PCRE2-10.49/publisher.json /out/new-scan
python3 -B -m unittest discover -s julia/qualification -p test_application_bundle.py
```

This second check binds the actual image config and manifest, the seven canonical package UUIDs in both Trivy and SPDX, eleven archive-derived native wrapper records, the packaged CA data owner, the classified Julia runtime in Syft and known-vulnerable HTTP and OpenSSL wrapper controls. It revalidates all nine published PCRE2 project advisory records against their actual source response, the current vendor security policy and both observed native versions. The vendor ranges identify seven records for the original 10.47 library. The same pass independently validates eight published CVE Program affected ranges and two NVD exact-version CPE responses. It refuses changed, stale or missing controls. The 8-bit upstream 10.49 identity is distinct from the 10.49.1+0 wrapper build. It keeps the unmodified results, refuses any applicable wrapper/package finding or critical OS finding, and reports all OS severities. The standard distro's full 23-owner checks remain active.

The production release sequence starts with approved native advisory coverage, an independently verified immutable source build and a reviewed base in the Eighred mirror. Its main-only publisher copies the upstream index and reads it back. A separate credential-limited consumer must establish access to the identical bytes before replacing the candidate's upstream base. The release owner then verifies signed application publication, immutable pull, bounded supervision and rollback; activation remains subject to the existing governance. [Mirror producer](../../../.github/workflows/base-image-mirror.yml) and [release issue](https://github.com/eighred/kanz/issues/1536)
