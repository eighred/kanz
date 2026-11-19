# JuliaC compiles in its copy of the prepared application project.
include(joinpath(dirname(Base.active_project()), "src", "Worker.jl"))

function @main(::Vector{String})::Cint
    input = Base.fdio("stdin", 0, false)
    output = Base.fdio("stdout", 1, false)
    try
        Worker.run(input, output)
        return 0
    catch
        diagnostic = Base.fdio("stderr", 2, false)
        write(diagnostic, "curve shadow worker refused request\n")
        flush(diagnostic)
        return 1
    end
end
