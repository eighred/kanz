# Use JuliaC's compile, link and bundle API from its pinned registered tool project.
using JuliaC
using Pkg
using SHA
using TOML
include(joinpath(@__DIR__, "protobuf", "verified_source.jl"))

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

function build(project, destination, replay...)
    length(replay) in (0, 2) || error("replay requires both an earlier output and its trusted receipt digest")
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
    tree = VerifiedProtoBuf.verify(package)
    codec = Dict(relpath(joinpath(directory, name), package) => file_digest(joinpath(directory, name))
                 for (directory, _, names) in walkdir(joinpath(package, "src"))
                 for name in names if endswith(name, ".jl"))
    entrypoint = joinpath(@__DIR__, "entrypoint.jl")
    inputs = Dict(
        "juliaVersion" => string(VERSION), "juliaCTree" => tool.tree_hash,
        "juliaExecutableSHA256" => file_digest(joinpath(Sys.BINDIR, "julia")),
        "sysimageSHA256" => file_digest(joinpath(Sys.BINDIR, "../lib/julia/sys.so")),
        "buildManifestSHA256" => file_digest(tools), "workerSources" => initial,
        "protobufTree" => tree, "protobufSources" => codec, "entrypointSHA256" => file_digest(entrypoint),
    )
    mktempdir(dirname(destination)) do temporary
        archive = joinpath(temporary, "worker.o.a")
        image = ImageRecipe(
            file=entrypoint, project=project, cpu_target="generic",
            output_type="--output-exe", trim_mode="safe", img_path=archive, verbose=true,
        )
        prior = nothing
        if isempty(replay)
            withenv("KANZ_JULIAC_WORKER_SOURCE" => joinpath(project, "src", "Worker.jl")) do
                compile_products(image)
            end
        else
            previous, expected = replay
            receipt = joinpath(previous, "build.toml")
            file_digest(receipt) == expected || error("untrusted compiler receipt")
            prior = TOML.parsefile(receipt)
            all(prior[key] == value for (key, value) in inputs) || error("compiler inputs differ")
            cp(joinpath(previous, "worker.o.a"), archive)
            file_digest(archive) == prior["compilerObjectSHA256"] || error("compiler output differs")
            image.instantiated_project = project
            # JuliaC adds this C shim after compiling the archive in compile_products.
            push!(image.extra_objects, JuliaC._compile_jl_options_shim(image.jl_options))
        end
        link = LinkRecipe(image_recipe=image, outname=joinpath(temporary, "curve-worker"), rpath="@bundle")
        link_products(link)
        output = mkdir(joinpath(temporary, "output"))
        bundle = joinpath(output, "bundle")
        bundle_products(BundleRecipe(link_recipe=link, output_dir=bundle))
        source_digests(project) == initial || error("worker source changed during build")
        VerifiedProtoBuf.verify(package) == tree || error("ProtoBuf source changed during build")
        report = merge(inputs, Dict(
            "compilerObjectSHA256" => file_digest(archive),
            "executableSHA256" => file_digest(joinpath(bundle, "bin", "curve-worker")),
        ))
        isnothing(prior) || report == prior || error("relinked output differs from compiler receipt")
        mv(archive, joinpath(output, "worker.o.a"))
        open(io -> TOML.print(io, report; sorted=true), joinpath(output, "build.toml"), "w")
        mv(output, destination)
    end
end

length(ARGS) in (2, 4) || error("usage: build.jl PROJECT NEW_OUTPUT [VERIFIED_OUTPUT RECEIPT_SHA256]")
build(ARGS...)
