# Compiled calibration application qualification

This source-only profile builds the existing Julia curve worker as a relocatable application using the upstream PackageCompiler API. The compiler, package manager and registry live in the builder. The executable reads one bounded canonical Protobuf request on stdin and writes its response on stdout. Go remains the numerical acceptance and promotion owner.

Use the Julia 1.13.3 compiler with the canonical locked worker depot, an ordinary C compiler, Python 3.11 or later and Git. Prestage the four package archives named in [tool inputs](tool_inputs.json) and a General registry archive named `General.tar.gz` from [commit 0f0f2984](https://github.com/JuliaRegistries/General/archive/0f0f298446cd34ff4f4228983bd8eeabe1f0a83e.tar.gz). Run the builder on a fresh private writable filesystem with no network, no production credentials and the repository source mounted read-only.

```sh
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
  -f julia/qualification/application_bundle/Dockerfile /out
python3 -B julia/qualification/application_bundle/corpus.py "$(cat /out/image-id)" "$(git rev-parse HEAD)"
```

The standard distribution's 23 JLL owners, positive controls and source-mapping requirements remain in its existing qualification. This profile has a different runtime inventory. Its final image requires its own complete native/advisory and OS ownership, actual ELF and SBOM coverage, signed publication and rollback evidence before release. The existing Eighred mirror currently selects the static and CC Debian 12 bases. Its normal reviewed source update, write-authorized publication and immutable mirror readback precede any organization deployment of a new base. [Base selection and publication](../../../.github/workflows/base-image-mirror.yml) and [Release owner](https://github.com/eighred/kanz/issues/1536)
