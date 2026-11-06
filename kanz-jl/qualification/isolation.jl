# Test-only code run by an explicit OCI entrypoint override, never the worker.
println(stderr, "KANZ_OCI_STAGE=imports")
using Test, Sockets, SHA, TOML, Pkg
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
    "packages"=>packages, "generated"=>generated, "sources"=>sources))
