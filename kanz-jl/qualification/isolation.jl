# Test-only code run by an explicit OCI entrypoint override, never the worker.
using Test, Sockets, SHA, TOML, Pkg
@test VERSION == v"1.13.3"
@test ccall(:geteuid, Cuint, ()) == 65532
@test ENV["JULIA_DEPOT_PATH"] == "/opt/julia-depot"
@test ENV["JULIA_LOAD_PATH"] == "@:@stdlib"
@test ENV["JULIA_PKG_OFFLINE"] == "true"
@test isempty(Sockets.getipaddrs()) # --network none: no non-loopback interface
@test strip(read("/sys/fs/cgroup/memory.max", String)) == "1073741824"
@test strip(read("/sys/fs/cgroup/pids.max", String)) == "64"
@test split(strip(read("/sys/fs/cgroup/cpu.max", String))) == ["100000", "100000"]
status = read("/proc/self/status", String)
@test occursin(r"(?m)^CapEff:\s+0000000000000000$", status)
@test occursin(r"(?m)^NoNewPrivs:\s+1$", status)
for path in ("/opt/kanz-jl/refuse-write", "/opt/julia-depot/refuse-write", "/refuse-write")
    @test_throws SystemError open(path, "w")
end
@test isfile("/opt/kanz-jl/source.sha")
manifest = TOML.parsefile("/opt/kanz-jl/Manifest.toml")
packages = Dict{String,Any}()
for (_, info) in Pkg.dependencies()
    entry = only(manifest["deps"][info.name])
    @test string(info.version) == entry["version"]
    if haskey(entry, "git-tree-sha1")
        # Pinned Pkg's Git tree implementation hashes installed files, not just
        # the expected PackageInfo tree_hash metadata.
        actual = bytes2hex(Pkg.GitTools.tree_hash(info.source))
        @test actual == entry["git-tree-sha1"]
        packages[info.name] = Dict("version"=>string(info.version), "treeSHA1"=>actual)
    end
end
@test Set(keys(packages)) == Set(["ProtoBuf", "BufferedStreams", "EnumX"])
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
