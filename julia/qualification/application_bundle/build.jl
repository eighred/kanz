# Build-only program. Run in an isolated Linux compiler with the complete locked
# worker depot, the pinned build tools and the immutable General registry.
using Pkg
using TOML
using PackageCompiler
using Libdl
using SHA

length(ARGS) == 3 || error("expected repository, new source staging directory and bundle output")
repo, work, output = abspath.(ARGS)
VERSION == v"1.13.3" || error("unexpected Julia compiler")
pkgversion(PackageCompiler) == v"2.4.5" || error("unexpected application bundler")
active_pcre = realpath(Libdl.dlpath(Libdl.dlopen("libpcre2-8")))
active_pcre == realpath(joinpath(Sys.BINDIR, "..", "lib", "julia", "libpcre2-8.so.0.16.1")) ||
    error("unexpected active PCRE2 library")
bytes2hex(open(sha256, active_pcre)) == "041e83a9aa6adee377ec80f12044036b6f53c6082297d8e639a210e3dbfaf262" ||
    error("unexpected patched PCRE2 compiler bytes")
get(ENV, "JULIA_PKG_OFFLINE", "") == "true" || error("offline package resolution required")
ispath(work) && error("source staging directory must be new")
ispath(output) && error("bundle output must be new")
app = joinpath(work, "julia")
mkpath(app)
mkpath(joinpath(work, "schemas"))
cp(joinpath(repo, "schemas", "proto"), joinpath(work, "schemas", "proto"))
for name in ["src", "test", "generate.jl", "Project.toml", "Manifest.toml"]
    cp(joinpath(repo, "julia", name), joinpath(app, name))
end
cp(joinpath(@__DIR__, "KanzCurveCandidate.jl"), joinpath(app, "src", "KanzCurveCandidate.jl"))

project_file = joinpath(app, "Project.toml")
manifest_file = joinpath(app, "Manifest.toml")
original_project = TOML.parsefile(project_file)
original_owners = TOML.parsefile(manifest_file)["deps"]
application = merge(original_project, Dict("name"=>"KanzCurveCandidate", "version"=>"0.1.0",
                                           "uuid"=>"9673acde-c590-4435-a649-6d96b7de7ece"))
open(io -> TOML.print(io, application; sorted=true), project_file, "w")
Pkg.offline(true)
Pkg.activate(app)
Pkg.resolve()
resolved = TOML.parsefile(manifest_file)["deps"]
Set(keys(resolved)) == union(keys(original_owners), ["KanzCurveCandidate"]) ||
    error("offline solver changed the application owner inventory")
all(resolved[name] == owner for (name, owner) in original_owners) ||
    error("offline solver changed a canonical locked package")
parsed_project = TOML.parsefile(project_file)
all(parsed_project[k] == v for (k, v) in original_project) ||
    error("offline solver changed the worker dependency contract")

for entry in [joinpath(app, "generate.jl"), joinpath(app, "test", "runtests.jl")]
    cmd = `$(Base.julia_cmd()) --startup-file=no --history-file=no --project=$app $entry`
    run(addenv(cmd, "JULIA_LOAD_PATH"=>"@:@stdlib"))
end
PackageCompiler.create_app(app, output; filter_stdlibs=true, incremental=false, cpu_target="generic",
                           executables=["curve-worker"=>"julia_main"])
metadata = joinpath(output, "share", "kanz")
mkpath(metadata)
for name in ["Project.toml", "Manifest.toml"]
    cp(joinpath(app, name), joinpath(metadata, name))
end
