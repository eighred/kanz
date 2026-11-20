# Safe-trimmed calibration application

This local candidate runs the existing CurveKernel and Worker through a single binary protobuf stdin/stdout exchange. The compiler consumes the prepared application project, including the ProtoBuf sources and the contracts generated from the repository schemas. The Go risk-engine continues to use its configured interpreter transport. This candidate has no deployment, promotion or execution authority.

## Compile

Prepare the [worker-specific ProtoBuf project](protobuf/README.md) in a disposable checkout first. The build checks the complete package tree before and after native compilation or replay and includes its identity in the compiler receipt.

The build-only JuliaC environment is independent of the application environment. Instantiate `tools/Project.toml` using its checked-in manifest. JuliaC 0.3.12 is pinned to registered Git tree `49a147f54e5f9ad46104834de93648dd9ef012f0` from commit `98e82861ff28c05d4eeabbf9de67c08f0bd97e80`. Its build-only ObjectFile 0.5.1, StructIO 0.3.1 and LIEF_Patchelf_jll 1.0.0+1 trees are recorded in that manifest. [Upstream bundling implementation](https://github.com/JuliaLang/JuliaC.jl/blob/98e82861ff28c05d4eeabbf9de67c08f0bd97e80/src/bundling.jl)

Supply a disposable Julia 1.13.3 compiler staged with the reviewed PCRE2 and libstdc++ builds. Rebuild the private Julia runtime and check its ABI against the original runtime and target libc. The source recipe pins the compiler, headers, static libraries and signed Julia inputs. [Native build and ABI qualification](https://github.com/eighred/kanz/tree/8fd830a93266adc03787672560eb4104f491aa9f/julia/qualification/application_bundle)

In the staged compiler copy, install the verified regular file as `lib/julia/libjulia-internal.so.1.13.3`. Preserve the existing symlinks. The build checks the [two rebuilt libraries](rebuilt_runtime.toml) in the compiler directory and in the final bundle. The replay receipt includes both digests.

Prepare `src/`, generated `gen/` and the selected ProtoBuf path in the worker manifest. Use an unused output directory. Set `JULIA_PKG_OFFLINE=true` and isolate the build after acquiring the pinned dependencies. From the repository root run:

```sh
"$JULIA" --startup-file=no --history-file=no --project=julia/qualification/safe_bundle/tools \
  julia/qualification/safe_bundle/build.jl "$PREPARED_WORKER_PROJECT" "$OUTPUT"
```

`build.jl` uses JuliaC's upstream compile, link and bundle phases. The compiler must pass `--trim=safe` with the generic CPU target. The upstream bundler selects the shipped libraries. `$OUTPUT/build.toml` records the object archive, linked executable, tool manifest, generated contract and implementation hashes. Preserve this receipt beside the normalized archive of `$OUTPUT/bundle`.

For independent relinking, supply the previous compiler output and the SHA-256 of its trusted build receipt. This mode rechecks the source inputs, retained `worker.o.a` and final executable digest. The fresh compile path uses a stable source mount. Two independent compilations of matching inputs can still emit different object bytes with this Julia release. [Upstream determinism investigation](https://github.com/JuliaLang/julia/issues/63149)

```sh
"$JULIA" --startup-file=no --history-file=no --project=julia/qualification/safe_bundle/tools \
  julia/qualification/safe_bundle/build.jl "$PREPARED_WORKER_PROJECT" "$REPLAY" "$OUTPUT" "$TRUSTED_RECEIPT_SHA256"
```

## Verify the packaged bytes

Use the pinned nonroot base and the local-only [application image recipe](https://github.com/eighred/kanz/blob/0922f9dba4ba37a1a94016c11291803b5741cd83/julia/qualification/application_bundle/Dockerfile). Stage the bundle as `bundle.tar` with ownership 65532:65532, bind the build revision through the OCI revision label and retain the exact image ID. On Linux the archive command is:

```sh
tar --sort=name --mtime=@0 --owner=65532 --group=65532 --numeric-owner --format=gnu \
  -C "$OUTPUT/bundle" -cf "$CONTEXT/bundle.tar" .
docker build --pull=false --network=none --platform=linux/amd64 \
  --label "org.eighred.calibration.test-bundle-sha256=$(sha256sum "$CONTEXT/bundle.tar" | cut -d' ' -f1)" \
  --label "org.opencontainers.image.revision=$SOURCE" --iidfile "$RECEIPTS/image.id" "$CONTEXT"
```

Export the filesystem of an unstarted container created from that immutable image using `docker export`. Remove the owned container after export. On Linux with `readelf`, compare the exported application files and links with the build archive, inspect every application and base ELF, and compare the application against the [previous 24-ELF candidate](https://github.com/eighred/kanz/issues/1536#issuecomment-7023736243). The recorded profile checks the reviewed libstdc++ replacement, removal of the compiler, interpreter and sysimage, and addition of OpenBLAS and its trampoline. Consumer header applicability and remaining native advisory coverage require separate evidence.

```sh
python3 -B julia/qualification/safe_bundle/inventory.py \
  "$CONTEXT/bundle.tar" "$RECEIPTS/rootfs.tar" "$RECEIPTS/inventory.json"
python3 -B -m unittest discover -s julia/qualification/safe_bundle -p test_inventory.py
```

With the generated Go SDK and local container engine, run the tracked direct wire oracle and real Go supervisor. Pass the image ID produced by `docker image inspect --format '{{.Id}}'` and the revision label on that image. The qualifier checks every base layer against the pinned base, then runs the 13 cases directly and through `shadowprocess.Run`. The supervisor checks the actual launcher SHA-256, deadline, child cleanup and scratch cleanup. The only additional mount for this second pass is the read-only test executable.

The quota control first submits the ordinary deposit fixture with one PID available. It checks the signal-listener startup refusal, exit code and empty response. The same image and request then run at the normal 64-PID limit, and the Go oracle verifies the resulting curve.

```sh
python3 -B julia/qualification/safe_bundle/qualify.py \
  "$IMAGE_ID" "$SOURCE" "$RECEIPTS/inventory.json" "$RECEIPTS/runtime.json"
```

The source for the local upstream base and the pending Eighred mirror are tracked in [#1845](https://github.com/eighred/kanz/pull/1845). Signing, registry publication, deployment, production rollback and regulated model promotion have their own acceptance requirements. [#1536](https://github.com/eighred/kanz/issues/1536)

## Attribute the compiler-free native libraries

Use the Linux tooling and advisory database specified in `qualification/advisory.py`. Run in the disposable, network-disabled scanner container with the OCI, publisher inputs, General registry snapshot and tool binaries mounted read-only. `TOOLS` holds each executable and its pinned release archive; `CACHE/db` holds the fresh Trivy database. Both commands accept filesystem paths.

```sh
python3 -B julia/qualification/safe_bundle/scan_blas.py \
  "$OCI" "$TOOLS" "$CACHE" "$GENERAL_REGISTRY_TOML" "$PREVIOUS_PUBLISHER_RECEIPT" "$NEW_SCAN_DIRECTORY"
python3 -B julia/qualification/safe_bundle/blas_owners.py \
  "$OCI" "$DISTROLESS_AMD64_MANIFEST" "$PREVIOUS_PUBLISHER_RECEIPT" \
  "$OPENBLAS_ARCHIVE" "$TRAMPOLINE_ARCHIVE" "$NEW_SCAN_DIRECTORY"
```

The second command verifies the immutable Debian base manifest and layer blobs, selects the linux/amd64 image and reads the actual application ELF bytes. Its preceding publisher receipt comes from the [24-ELF compiled image](https://github.com/eighred/kanz/issues/1536#issuecomment-7023736243); the verified sixteen retained publisher files and rebuilt GCC runtime have independent owner records. The Julia runtime and first-party executable remain under their own runtime qualification. These component checks do not approve the release.

| Shipped owner | Selected binary and build provenance |
| --- | --- |
| OpenBLAS_jll 0.3.30+0 | [x86_64 glibc / libgfortran5 artifact](https://github.com/JuliaBinaryWrappers/OpenBLAS_jll.jl/blob/ec1bca51ecf2a5d5a556073fbacedeae26b9d7a7/Artifacts.toml), [Yggdrasil recipe](https://github.com/JuliaPackaging/Yggdrasil/blob/32d6c14d4a0eb04133420df0903f3b46b6a1f931/O/OpenBLAS/common.jl). The official [OpenBLAS 0.3.30 source archive](https://github.com/OpenMathLib/OpenBLAS/releases/tag/v0.3.30) hashes to `27342cff518646afb4c2b976d809102e368957974c250a25ccc965e53063c95d`. Its bundled LAPACK `INSTALL/ilaver.f` reports 3.12.0. |
| libblastrampoline_jll 5.15.0+0 | [x86_64 glibc artifact](https://github.com/JuliaBinaryWrappers/libblastrampoline_jll.jl/blob/8e76ae53eb42716ae1ad732a48a67392b5a562e8/Artifacts.toml), [Yggdrasil recipe](https://github.com/JuliaPackaging/Yggdrasil/blob/f0d8e131bedfec88a99c3e51722e0e7c1d6f45d2/L/libblastrampoline/common.jl) and its [upstream source commit](https://github.com/JuliaLinearAlgebra/libblastrampoline/commit/072b5f67895bec0b92f8c83194567c1c48e9833d). |

The actual image scanner and Syft inventory are accompanied by a byte-bound JLL metadata scan. Ten retained JLL identities are rescanned against the same database. A metadata-only positive control for the same OpenBLAS UUID at 0.3.13+0 must report CVE-2021-4048. The Julia advisory lists the JLL fix at 0.3.20+0; the upstream fix precedes the selected OpenBLAS source commit. The HTTP and OpenSSL controls run independently. A zero-finding JLL result covers the selected scanner database and identities. Native advisory sources can have different product coverage. [Julia advisory](https://osv.dev/vulnerability/JLSEC-2025-6) Â· [Upstream fix ancestry](https://github.com/OpenMathLib/OpenBLAS/compare/fe497efa0510466fd93578aaf9da1ad8ed4edbe7...993fad6aebbce34a97d3f8c34d6d79d35b64cc48)

## Ubuntu Chisel comparison

The optional amd64 profile reuses the two application-layer descriptors of the reviewed OCI at platform manifest `a0b0d9f1e6dd6f94c6ad5208d38ae86e42c5d69912b3b935bed935b97b829a39`. The Go caller and the default Debian checks keep their original entrypoints. The [Canonical slice definitions](https://github.com/canonical/chisel-releases/blob/a84c5eb70f22c34212c6582f603467498061c978/slices/libc6.yaml) separate `libc6_libs` from the optional `libc6_gconv` plugins.

1. Run `bash chisel_cut.sh EMPTY_DIRECTORY` in a disposable Linux amd64 builder with GNU tar, bash, curl, file, dpkg-deb and sha256sum. The script verifies the pinned Chisel v1.5.1, Noble definitions and Canonical rocks-toolbox v1.2.1 inputs. Chisel verifies the normal Ubuntu archive signatures. The output is a base tar and a separate converter control from the same signed libc6 package.
2. Build and import the independent candidate with `python chisel_image.py ORIGINAL_OCI BASE_TAR CANDIDATE_OCI` and `docker load --input CANDIDATE_OCI`. Save that immutable local image with `docker image save --output SCANNABLE_TAR IMAGE_ID`. Keep the printed platform manifest for the readback.
3. Run the existing `scan_blas.py SCANNABLE_TAR PINNED_TOOLS TRIVY_CACHE GENERAL_REGISTRY PREVIOUS_RECEIPT SCANS`. Use the same original publisher receipt, exact scanner binaries and current database as the Debian profile. No scanner exclusions apply.
4. With Python 3.14+, the prepared Go SDK and Docker, run `python chisel_verify.py ORIGINAL_OCI SCANNABLE_TAR BASE_TAR CONTROL_TAR PREVIOUS_RECEIPT SCANS IMAGE_ID`. The verifier checks manifest file digests and symlinks, the actual dpkg, Trivy and Syft package revisions, the four positive-control converter binaries, the unchanged application layers and the authenticated archive identities. It then runs the ordinary direct oracle and Go supervisor corpus with the same isolation and lifecycle checks.

The qualified cut uses seven packages including libc6 `2.39-0ubuntu8.12`. The verifier pins the architecture, version and SHA-256 of each signed Ubuntu binary package. A package update requires a new owner review, cut, scan and worker qualification before these pins change. Both scanners must enumerate all seven. A zero-finding scan with no OS inventory fails verification. The four optional converter files associated with CVE-2026-121757, CVE-2026-121758 and CVE-2026-124024 are absent from this cut. Ubuntu lists the source package as awaiting evaluation. The Noble security/update pocket supplied timezone data 2026e. Record the dated advisory review, limitations and release decision in [issue #1536](https://github.com/eighred/kanz/issues/1536).
