using TOML
using ProtoBuf

VERSION == v"1.13.3" || error("Julia 1.13.3 is required")
project_dir = realpath(joinpath(@__DIR__, ".."))
realpath(Base.active_project()) == joinpath(project_dir, "Project.toml") ||
    error("the selected project must own the worker")
length(DEPOT_PATH) == 1 || error("the offline depot must be the only depot")
LOAD_PATH == ["@", "@stdlib"] || error("only the worker project and stdlib may resolve packages")
depot = realpath(only(DEPOT_PATH))
manifest = TOML.parsefile(joinpath(project_dir, "Manifest.toml"))["deps"]
sources = Set{String}()

for (name, records) in manifest
    entry = only(records)
    haskey(entry, "git-tree-sha1") || continue
    id = Base.PkgId(Base.UUID(entry["uuid"]), name)
    slug = Base.version_slug(id.uuid, Base.SHA1(entry["git-tree-sha1"]))
    source = joinpath(depot, "packages", name, slug, "src", name * ".jl")
    resolved = Base.locate_package(id)
    !isnothing(resolved) && realpath(resolved) == realpath(source) ||
        error("package resolution differs for " * name)
    loaded = get(Base.loaded_modules, id, nothing)
    !isnothing(loaded) && realpath(pathof(loaded)) == realpath(source) ||
        error("the worker import did not load the staged " * name)
    Base.pkgversion(loaded) == VersionNumber(entry["version"]) ||
        error("loaded package version differs for " * name)
    cache = joinpath(depot, "compiled", "v$(VERSION.major).$(VERSION.minor)", name)
    isdir(cache) && any(f -> endswith(f, ".ji") && filesize(joinpath(cache, f)) > 0, readdir(cache)) ||
        error("the prepared depot has no compiled package cache for " * name)
    push!(sources, name)
end

sources == Set(["BufferedStreams", "EnumX", "ProtoBuf"]) ||
    error("the worker's external source inventory changed")
println("Resolved and loaded the three pinned packages from the offline depot")
