using TOML
include(joinpath(@__DIR__, "verified_source.jl"))

# Apply only in an isolated copy of the worker project.
length(ARGS) == 3 || error("usage: select_project.jl BASE_MANIFEST PACKAGE_DIR NEW_MANIFEST")
source, package, output = ARGS
ispath(output) && error("manifest output already exists")
package = realpath(package)
VerifiedProtoBuf.verify(package)
manifest = TOML.parsefile(source)
entry = only(manifest["deps"]["ProtoBuf"])
entry["uuid"] == "3349acd9-ac6a-5e09-bcdb-63829b23a429" || error("unexpected ProtoBuf UUID")
entry["version"] == "1.3.0" || error("unexpected ProtoBuf baseline")
pop!(entry, "git-tree-sha1") == "da18083a52d9d57bbe6dadaacad39731e5f7be39" || error("unexpected ProtoBuf source")
delete!(entry, "registries")
entry["path"] = package
open(io -> TOML.print(io, manifest; sorted=true), output, "w")
