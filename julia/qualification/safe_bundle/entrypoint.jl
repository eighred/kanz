# JuliaC's temporary project selects dependencies. Use the prepared source at
# its stable build mount so compiler metadata does not contain a random copy path.
include(ENV["KANZ_JULIAC_WORKER_SOURCE"])

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
