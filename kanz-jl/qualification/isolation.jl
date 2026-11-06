# Test-only code run by an explicit OCI entrypoint override, never the worker.
println(stderr, "KANZ_OCI_STAGE=imports")
using Test, Sockets, SHA, TOML, Pkg, Libdl
println(stderr, "KANZ_OCI_STAGE=runtime")
@test VERSION == v"1.13.3"
@test ccall(:geteuid, Cuint, ()) == 65532
@test ENV["JULIA_DEPOT_PATH"] == "/opt/julia-depot"
@test ENV["JULIA_LOAD_PATH"] == "@:@stdlib"
@test ENV["JULIA_PKG_OFFLINE"] == "true"
@test isempty(Sockets.getipaddrs()) # --network none: no non-loopback interface
println(stderr, "KANZ_OCI_STAGE=cgroup")
@test strip(read("/sys/fs/cgroup/memory.max", String)) == "1073741824"
@test strip(read("/sys/fs/cgroup/pids.max", String)) == "64"
@test split(strip(read("/sys/fs/cgroup/cpu.max", String))) == ["100000", "100000"]
println(stderr, "KANZ_OCI_STAGE=process-security")
status = read("/proc/self/status", String)
@test occursin(r"(?m)^CapEff:\s+0000000000000000$", status)
@test occursin(r"(?m)^NoNewPrivs:\s+1$", status)
println(stderr, "KANZ_OCI_STAGE=write-refusal")
for path in ("/opt/kanz-jl/refuse-write", "/opt/julia-depot/refuse-write", "/refuse-write")
    @test_throws SystemError open(path, "w")
end
@test isfile("/opt/kanz-jl/source.sha")
println(stderr, "KANZ_OCI_STAGE=installed-packages")
excluded_environments = ["base/JuliaSyntax/docs", "stdlib/v1.13/TOML/docs",
    "stdlib/v1.13/TOML/benchmark", "stdlib/v1.13/Tar/test", "test"]
for path in excluded_environments
    @test !ispath(joinpath("/opt/julia/share/julia", path))
    @test !islink(joinpath("/opt/julia/share/julia", path))
end
manifest = TOML.parsefile("/opt/kanz-jl/Manifest.toml")
packages = Dict{String,Any}()
# Pkg.dependencies() constructs EnvCache and writes depot usage locks. Resolve
# installed code through Julia's loader without making the read-only depot writable.
for (name, entries) in manifest["deps"]
    entry = only(entries)
    source = Base.locate_package(Base.PkgId(Base.UUID(entry["uuid"]), name))
    @test source !== nothing
    source = realpath(source)
    root = realpath(dirname(dirname(source)))
    @test startswith(source, root * "/src/")
    if haskey(entry, "git-tree-sha1")
        @test dirname(dirname(root)) == "/opt/julia-depot/packages"
        @test basename(dirname(root)) == name
    else
        @test root == "/opt/julia/share/julia/stdlib/v1.13/" * name
    end
    installed = TOML.parsefile(joinpath(root, "Project.toml"))
    @test installed["uuid"] == entry["uuid"]
    @test installed["version"] == entry["version"]
    if haskey(entry, "git-tree-sha1")
        actual = bytes2hex(Pkg.GitTools.tree_hash(root))
        @test actual == entry["git-tree-sha1"]
        packages[name] = Dict("version"=>installed["version"], "treeSHA1"=>actual)
    end
end
@test Set(keys(packages)) == Set(["ProtoBuf", "BufferedStreams", "EnumX"])
# Independent host code compares this complete selected runtime inventory with
# the checksum-verified official archive. No native advisory ownership is inferred.
println(stderr, "KANZ_OCI_STAGE=native-runtime")
native_files = Dict{String,Any}()
native_links = Dict{String,String}()
native_jll = Dict{String,Any}()
native_total = Ref(0)
function bounded_hash(path, maximum)
    @test isfile(path) && !islink(path)
    size = filesize(path)
    @test 0 <= size <= maximum
    native_total[] += size
    @test native_total[] <= 2^31
    Dict("size"=>size, "sha256"=>bytes2hex(open(sha256, path)))
end
for directory in ("bin", "lib", "libexec", "share/julia/compiled")
    @test realpath(joinpath("/opt/julia", directory)) == joinpath("/opt/julia", directory)
    for (dir, dirs, files) in walkdir(joinpath("/opt/julia", directory); follow_symlinks=false)
        @test all(!islink(joinpath(dir, d)) for d in dirs)
        for file in files
            path = joinpath(dir, file)
            relative = relpath(path, "/opt/julia")
            @test length(native_files) + length(native_links) < 4096
            if islink(path)
                target = readlink(path)
                @test !isabspath(target) && !occursin('\\', target)
                @test startswith(realpath(path), "/opt/julia/")
                native_links[relative] = target
            else
                native_files[relative] = bounded_hash(path, 768 * 2^20)
            end
        end
    end
end
for name in readdir("/opt/julia/share/julia/stdlib/v1.13")
    endswith(name, "_jll") || continue
    @test length(native_jll) < 128
    path = joinpath("/opt/julia/share/julia/stdlib/v1.13", name, "Project.toml")
    @test realpath(path) == path
    info = bounded_hash(path, 65536)
    meta = TOML.parsefile(path)
    @test meta["name"] == name
    for field in ("name", "uuid", "version")
        info[field] = meta[field]
    end
    native_jll[name] = info
end
generated_native = Dict{String,Any}()
for (dir, dirs, files) in walkdir("/opt/julia-depot/compiled"; follow_symlinks=false)
    @test all(!islink(joinpath(dir, d)) for d in dirs)
    for file in files
        endswith(file, ".so") || continue
        path = joinpath(dir, file)
        @test realpath(path) == path
        @test length(generated_native) < 4096
        generated_native[relpath(path, "/opt/julia-depot")] = bounded_hash(path, 768 * 2^20)
    end
end
outside = Dict{String,Any}()
native = Dict("files"=>native_files, "links"=>native_links, "jll"=>native_jll,
    "generatedNativeImages"=>generated_native,
    "unassignedFiles"=>sort!(vcat(collect(keys(native_files)), ["depot/" * p for p in keys(generated_native)])),
    "loadedOutsideArchive"=>outside,
    "coverage"=>"native-library-advisory-ownership-unqualified")
# Execute only the archive-bound installed utility's information command under
# the same OCI limits. Do not infer native version from the compatibility JLL stub.
println(stderr, "KANZ_OCI_STAGE=native-version")
seven_path = "/opt/julia/libexec/julia/7z"
@test realpath(seven_path) == seven_path && !islink(seven_path)
@test native_files["libexec/julia/7z"]["sha256"] == "a15c6706c64672f07d4cc7358cc7eee3aa0d0f7cfe3fdfcf31deb808d3b0b8ee"
seven_output = open(`$seven_path i`, "r") do io
    bytes = read(io, 65537)
    @test length(bytes) <= 65536
    String(bytes)
end
@test occursin(r"(?m)^7-Zip \(a\) 26\.03 \(x64\) : Copyright \(c\) 1999-2026 Igor Pavlov : 2026-09-03\r?$", seven_output)
seven = Dict("path"=>"libexec/julia/7z", "sha256"=>native_files["libexec/julia/7z"]["sha256"],
    "version"=>"26.03", "versionCommand"=>"i")
println(stderr, "KANZ_OCI_STAGE=openssl-version")
openssl_files = Dict{String,Any}()
for (name, expected) in (("libcrypto.so.3", "09ac6b8640f13b13badf5d2430a8cd326d27ad77668868ad6c9a36de87c4857d"),
                         ("libssl.so.3", "044e0615a9f42e6e02298eec073325f528bee55c56901134788220fc5e96a098"))
    path = "/opt/julia/lib/julia/" * name
    @test realpath(path) == path && !islink(path)
    @test native_files["lib/julia/" * name]["sha256"] == expected
    openssl_files["lib/julia/" * name] = native_files["lib/julia/" * name]
end
crypto = Libdl.dlopen("/opt/julia/lib/julia/libcrypto.so.3")
ssl = Libdl.dlopen("/opt/julia/lib/julia/libssl.so.3")
@test realpath(Libdl.dlpath(crypto)) == "/opt/julia/lib/julia/libcrypto.so.3"
@test realpath(Libdl.dlpath(ssl)) == "/opt/julia/lib/julia/libssl.so.3"
@test unsafe_string(ccall(Libdl.dlsym(crypto, :OpenSSL_version), Cstring, (Cint,), 0)) == "OpenSSL 3.5.9 29 Sep 2026"
# Exercise the linked ssl/crypto pair without creating a socket or TLS bypass.
method = ccall(Libdl.dlsym(ssl, :TLS_method), Ptr{Cvoid}, ())
ssl_context = ccall(Libdl.dlsym(ssl, :SSL_CTX_new), Ptr{Cvoid}, (Ptr{Cvoid},), method)
@test ssl_context != C_NULL
ccall(Libdl.dlsym(ssl, :SSL_CTX_free), Cvoid, (Ptr{Cvoid},), ssl_context)
openssl_license = bytes2hex(sha256(read("/opt/julia/share/licenses/kanz-openssl-runtime/LICENSE.txt")))
@test openssl_license == "7d5450cb2d142651b8afa315b5f238efc805dad827d91ba367d8516bc9d49e7a"
openssl = Dict("version"=>"3.5.9", "versionAPI"=>"OpenSSL_version(0)",
    "archiveSHA256"=>"8e2c75378726017fc66fb7fbbd10fb53522417c3e45a88af77b8853d46f2b063",
    "files"=>openssl_files, "licenseSHA256"=>openssl_license, "originalJLLVersion"=>native_jll["OpenSSL_jll"]["version"])
for loaded in Libdl.dllist()
    loaded == "linux-vdso.so.1" && continue
    path = realpath(loaded)
    if startswith(path, "/opt/julia/")
        @test haskey(native_files, relpath(path, "/opt/julia"))
        continue
    elseif startswith(path, "/opt/julia-depot/")
        @test haskey(generated_native, relpath(path, "/opt/julia-depot"))
        continue
    end
    @test occursin(r"^/(usr/)?lib(64)?/[A-Za-z0-9_./+-]{1,256}$", path)
    @test length(outside) < 64
    outside[path] = bounded_hash(path, 128 * 2^20)
end
@test !isempty(outside)
Libdl.dlclose(ssl)
Libdl.dlclose(crypto)
println(stderr, "KANZ_OCI_STAGE=inventory")
generated = Dict{String,String}()
for (dir, _, files) in walkdir("/opt/kanz-jl/gen")
    for file in files
        path = joinpath(dir,file)
        generated[relpath(path,"/opt/kanz-jl/gen")] = bytes2hex(sha256(read(path)))
    end
end
@test !isempty(generated)
sources = Dict{String,String}()
for directory in ("src", "bin", "test", "qualification")
    for (dir, _, files) in walkdir(directory)
        for file in files
            path = joinpath(dir,file)
            sources[path] = bytes2hex(sha256(read(path)))
        end
    end
end
TOML.print(stdout, Dict("sourceSHA"=>read("source.sha",String), "runtime"=>string(VERSION),
    "projectSHA256"=>bytes2hex(sha256(read("Project.toml"))),
    "manifestSHA256"=>bytes2hex(sha256(read("Manifest.toml"))),
    "runtimeExecutableSHA256"=>bytes2hex(sha256(read("/opt/julia/bin/julia"))),
    "nativeRuntime"=>native, "native7zip"=>seven, "nativeOpenSSL"=>openssl, "excludedDevelopmentEnvironments"=>excluded_environments, "packages"=>packages, "generated"=>generated, "sources"=>sources))
