using Test

include(joinpath(@__DIR__, "verified_source.jl"))

# Keep executable bits intact because they contribute to the package tree hash.
function with_readonly_checkout(f, checkout)
    modes = Pair{String, Int}[]
    try
        for (directory, _, files) in walkdir(checkout; topdown=false)
            for path in [joinpath.(Ref(directory), files); directory]
                islink(path) && continue
                mode = Int(filemode(path)) & 0o7777
                push!(modes, path => mode)
                chmod(path, mode & 0o555)
            end
        end
        f()
    finally
        for (path, mode) in Iterators.reverse(modes)
            chmod(path, mode)
        end
    end
end

length(ARGS) == 1 || error("usage: test_verified_source.jl PATCHED_PROTOBUF_CHECKOUT")

@testset "selected ProtoBuf source" begin
    mktempdir() do scratch
        package = joinpath(scratch, "ProtoBuf")
        cp(realpath(only(ARGS)), package; follow_symlinks=false)
        @test VerifiedProtoBuf.verify(package) == VerifiedProtoBuf.WORKER_TREE

        @testset "read-only checkout" begin
            with_readonly_checkout(package) do
                @test iszero(filemode(joinpath(package, ".git", "index")) & 0o222)
                @test VerifiedProtoBuf.verify(package) == VerifiedProtoBuf.WORKER_TREE
            end
        end

        @testset "working bytes" begin
            for relative in ("README.md", joinpath("src", "codec", "decode.jl"))
                path = joinpath(package, relative)
                original = read(path)
                open(io -> println(io, "\n# qualification fixture"), path, "a")
                @test_throws ErrorException("ProtoBuf package bytes differ") VerifiedProtoBuf.verify(package)
                write(path, original)
            end
            @test VerifiedProtoBuf.verify(package) == VerifiedProtoBuf.WORKER_TREE
        end

        @testset "index differs while working bytes match" begin
            path = joinpath(package, "README.md")
            original = read(path)
            open(io -> println(io, "\nQualification fixture."), path, "a")
            run(`git -C $package add -- README.md`)
            write(path, original)
            @test_throws ErrorException("applied ProtoBuf source tree differs") VerifiedProtoBuf.verify(package)
        end
    end
end
