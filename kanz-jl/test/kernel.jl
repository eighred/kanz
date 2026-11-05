using Test
include(joinpath(@__DIR__, "..", "src", "CurveKernel.jl"))
using .CurveKernel
inputquote(kind, tenor, rate; span=0.0) = (; kind, tenor=Float64(tenor), span=Float64(span), rate=Float64(rate))

@testset "Independent rational discount factors" begin
    t,z = calibrate([inputquote(1,1,0.05)])
    @test discount(t,z,1) ≈ Float64(20//21) atol=1e-14 rtol=0
    t,z = calibrate([inputquote(2,1,0.04;span=0.25),inputquote(1,1,0.05)])
    @test discount(t,z,1.25) ≈ Float64(2000//2121) atol=1e-14 rtol=0
    t,z = calibrate([inputquote(1,1,0.05),inputquote(3,2,0.06)])
    @test discount(t,z,2) ≈ Float64(330//371) atol=1e-13 rtol=0
    t,z = calibrate([inputquote(1,1,-0.05)])
    @test discount(t,z,1) ≈ Float64(20//19) atol=1e-14 rtol=0
    @test z[1] < 0
    t,z = calibrate([inputquote(3,0.5,0.04)])
    @test discount(t,z,0.5) ≈ Float64(50//51) atol=1e-13 rtol=0
end
@testset "Refusal boundaries" begin
    @test_throws InvalidInput calibrate([])
    @test_throws InvalidInput calibrate([inputquote(2,1,0.04)])
    @test_throws InvalidInput calibrate([inputquote(1,1,0.04),inputquote(3,1,0.05)])
    @test_throws InvalidInput calibrate([inputquote(1,101,0.04)])
    @test_throws InvalidInput calibrate([inputquote(1,0,0.04)])
    for bad in (NaN, Inf, -Inf)
        @test_throws InvalidInput calibrate([inputquote(1,1,bad)])
        @test_throws InvalidInput calibrate([inputquote(1,bad,0.04)])
        @test_throws InvalidInput calibrate([inputquote(2,1,0.04;span=bad)])
    end
    @test_throws NumericalFailure calibrate([inputquote(1,1,-1)])
    @test_throws NumericalFailure calibrate([inputquote(3,1,10)])
    @test_throws InvalidInput calibrate(fill(inputquote(1,1,0.04),257))
    t,z = calibrate([inputquote(1,1,0.05),inputquote(2,1,0.04;span=-1)])
    @test discount(t,z,1.25) ≈ Float64(2000//2121) atol=1e-14 rtol=0
end
@testset "Unsorted mixed strip reprices captured instruments" begin
    t,z = calibrate([inputquote(3,3,0.055),inputquote(1,0.5,0.03),
                    inputquote(2,0.5,0.04;span=0.5),inputquote(3,2,0.05)])
    @test issorted(t)
    @test (1/discount(t,z,0.5)-1)/0.5 ≈ 0.03 atol=1e-12 rtol=0
    @test (discount(t,z,0.5)/discount(t,z,1)-1)/0.5 ≈ 0.04 atol=1e-12 rtol=0
    for (tenor, rate) in ((2,0.05),(3,0.055))
        fixed = rate * sum(discount(t,z,Float64(year)) for year in 1:tenor)
        @test fixed + discount(t,z,Float64(tenor)) ≈ 1.0 atol=1e-12 rtol=0
    end
    # Flat zero extrapolation, not flat DF, on both tails.
    @test discount(t,z,0.25) ≈ exp(-z[1]*0.25) atol=1e-14 rtol=0
    @test discount(t,z,4) ≈ exp(-z[end]*4) atol=1e-14 rtol=0
end
