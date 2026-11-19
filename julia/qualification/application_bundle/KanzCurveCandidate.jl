module KanzCurveCandidate

include("Worker.jl")

function julia_main()::Cint
    try
        Worker.run(stdin, stdout)
        return 0
    catch
        println(stderr, "curve shadow worker refused request")
        return 1
    end
end

end
