# Use JuliaC's compile, link and bundle API from its pinned registered tool project.
using JuliaC
using Pkg
using SHA
using TOML

file_digest(path) = bytes2hex(open(sha256, path))

function source_digests(project)
    files = ["Project.toml", "Manifest.toml"]
    for part in ("src", "gen")
        for (directory, _, names) in walkdir(joinpath(project, part)), name in names
            endswith(name, ".jl") && push!(files, relpath(joinpath(directory, name), project))
        end
    end
    return Dict(path => file_digest(joinpath(project, path)) for path in files)
end

function build(project, destination)
    VERSION == v"1.13.3" || error("Julia 1.13.3 required")
    Base.active_project() == joinpath(@__DIR__, "tools", "Project.toml") ||
        error("use the pinned safe_bundle/tools project")
    tool = Pkg.dependencies()[Base.PkgId(JuliaC).uuid]
    tool.version == v"0.3.12" && tool.tree_hash == "49a147f54e5f9ad46104834de93648dd9ef012f0" ||
        error("unexpected JuliaC source")
    project = realpath(project)
    destination = abspath(destination)
    ispath(destination) && error("output directory exists")
    isfile(joinpath(project, "src", "Worker.jl")) || error("missing worker source")
    initial = source_digests(project)
    tools = joinpath(@__DIR__, "tools", "Manifest.toml")
    # Record the path dependency actually selected by the prepared candidate.
    protobuf = only(TOML.parsefile(joinpath(project, "Manifest.toml"))["deps"]["ProtoBuf"])
    package = realpath(joinpath(project, protobuf["path"]))
    codec = Dict(relpath(joinpath(directory, name), package) => file_digest(joinpath(directory, name))
                 for (directory, _, names) in walkdir(joinpath(package, "src"))
                 for name in names if endswith(name, ".jl"))
    mktempdir(dirname(destination)) do temporary
        archive = joinpath(temporary, "worker.o.a")
        image = ImageRecipe(
            file=joinpath(@__DIR__, "entrypoint.jl"), project=project, cpu_target="generic",
            output_type="--output-exe", trim_mode="safe", img_path=archive, verbose=true,
        )
        compile_products(image)
        link = LinkRecipe(image_recipe=image, outname=joinpath(temporary, "curve-worker"), rpath="@bundle")
        link_products(link)
        output = mkdir(joinpath(temporary, "output"))
        bundle = joinpath(output, "bundle")
        bundle_products(BundleRecipe(link_recipe=link, output_dir=bundle))
        source_digests(project) == initial || error("worker source changed during build")
        report = Dict(
            "juliaVersion" => string(VERSION), "juliaCTree" => tool.tree_hash,
            "buildManifestSHA256" => file_digest(tools), "workerSources" => initial,
            "protobufSources" => codec, "entrypointSHA256" => file_digest(image.file),
            "compilerObjectSHA256" => file_digest(archive),
            "executableSHA256" => file_digest(joinpath(bundle, "bin", "curve-worker")),
        )
        open(io -> TOML.print(io, report; sorted=true), joinpath(output, "build.toml"), "w")
        mv(output, destination)
    end
end

length(ARGS) == 2 || error("usage: build.jl PREPARED_WORKER_PROJECT NEW_OUTPUT_DIRECTORY")
build(ARGS...)
