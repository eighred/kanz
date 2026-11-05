# No Pkg, network, feed lookup, database, credential or publication access.
# The supervising Go process owns deadlines, scrubbed environment and isolation.
try
    VERSION == v"1.13.3" || error("unsupported runtime")
    include(joinpath(@__DIR__, "..", "src", "Worker.jl"))
    Worker.run(stdin, stdout)
catch
    # Never render request values, paths, credentials or exception contents.
    println(stderr, "curve shadow worker refused request")
    exit(1)
end
