# Reproducible OpenSSL source input

This producer prepares reviewable `libcrypto.so.3` and `libssl.so.3` input for
#1536. It does not install them into Julia, qualify the consumer image, evaluate
advisory acceptance, publish a release or change a native/deployment hold.
The existing installer, scanners, Julia constraints and consumer workflow remain
the adoption boundary.

`pins.json` selects an immutable OpenSSL maintenance commit and codeload archive
hash. The source's actual identity is **3.5.10-dev**; `VERSION.dat` is checked
byte-for-byte and never rewritten. The commit is unsigned. Its hash and HTTPS
provenance are reviewed input identities, not a signed upstream release claim.
The producer rechecks that all five vendor CNA-linked fixes are ancestors of the
selected source. Unknown/missing responses fail; source provenance does not
establish artifact or consumer safety.

The compiler is the existing digest-pinned Rust/bookworm image used by the Julia
builder and `base-image-mirror.yml`. Two separate containers start with separate
empty, size-limited scratch filesystems. They receive no credentials, Docker
socket, host home, external network or mutable toolchain. Only the input mount
and immutable image are readable; writes go to scratch. The parent checks the
actual image/configuration, uid, mounts, environment command, cgroup limits and
ulimits. Missing gcc, Perl, make or binutils refuses the build; it never installs
tools or substitutes the WSL host compiler.

Both builds run OpenSSL's full verbose test suite and a separate selected recipe
run. Required non-skipped named cases are EC invalid-order key generation, the
five AES-GCM-SIV AAD reset/reinitialization cases, and the QUIC covering-range
regression. The EVP recipe includes upstream invalid-IV vectors. `test_bio_dgram`
provides current generic coverage; CVE-2026-88822 has no dedicated new upstream
regression in this source. These observations are not a complete CVE proof.
Verbose skip lines and upstream summaries are retained in each receipt.

The parent never loads a candidate library. Bounded ELF parsing independently
checks ELF64/amd64, SONAME3, required exported API symbols, dependency names and
absence of embedded runtime search paths. The archive must contain exactly the
two library bytes and upstream license. Both clean builds must produce identical
library/archive, license, compiler/sysroot inventory, configuration and actual
version bytes. Compiler/tool hashes, dpkg versions and immutable image/rootfs
identity bind the build environment. Reproducibility on this one environment
does not establish ABI compatibility with Julia or another libc baseline.

After each container stops, protected descriptor-relative reads refuse linked,
nonregular, oversized or changing outputs. Evidence is written exclusively
through a retained destination descriptor; the container never mounts that
destination. Cleanup observes and removes only the generated container name
with its matching source label, verifies absence, and unmounts owned scratch.
A failed ownership/daemon check leaves its unique scratch path for repair rather
than recursively deleting a possibly mounted tree. A success receipt exists only
after both complete builds, comparisons and observed cleanups.

The dedicated workflow runs only on the named candidate branch's push. It has
read-only content/package permissions, logs out after the pinned image pull and
does not dispatch the unchanged Julia qualification. Root review of its exact
head precedes a push. Creating a later PR naturally selects existing consumer
workflows; that requires the separate native-input/hold decision.

Unit guards run with `bash sandbox_tests.sh` on a Linux host with systemd,
bubblewrap and sudo. The harness first supports a `--probe` using only `true`,
then executes Python tests as nobody in an empty network/PID/user namespace,
read-only distro/source mounts and bounded tmpfs/cgroup/ulimits. It exposes no
host home or repository siblings. The actual producer requires Docker plus
permission to mount its bounded tmpfs; unavailable isolation is a refusal.
