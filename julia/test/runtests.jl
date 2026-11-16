include("kernel.jl")
include(joinpath(@__DIR__, "..", "src", "Worker.jl"))
using .Worker
using ProtoBuf

@testset "Generated wire boundary" begin
    R = Worker.R
    F = Worker.F
    stamp = Worker.research.google.protobuf.Timestamp(seconds=1700000000)
    input = R.CalibrationInput(instrument_id="deposit-1y", kind=R.RateQuoteKind.RATE_QUOTE_KIND_DEPOSIT,
        tenor_years=1.0, rate=OneOf(:resolved_rate,0.05))
    job = R.CurveCalibrationJob(capture_id="capture-1", capture_digest=fill(UInt8(1),32),
        currency="USD", requested_as_of=stamp, captured_at=stamp,
        interpolation=Worker.research.reference.v1.Interpolation.INTERPOLATION_LINEAR_ZERO, inputs=[input])
    request, response = IOBuffer(), IOBuffer()
    encode(ProtoEncoder(request),job)
    seekstart(request)
    Worker.run(request,response)
    seekstart(response)
    result = decode(ProtoDecoder(response),R.CurveCalibrationResult)
    @test result.capture_id == job.capture_id
    @test result.capture_digest == job.capture_digest
    @test result.failure == F.CURVE_CALIBRATION_FAILURE_NONE
    @test length(result.pillars) == 1
    @test exp(-result.pillars[1].zero_rate) ≈ Float64(20//21) atol=1e-14 rtol=0
    @test Worker.evaluate(R.CurveCalibrationJob()).failure == F.CURVE_CALIBRATION_FAILURE_INVALID_INPUT
    bad_convention = R.CurveCalibrationJob(capture_id=job.capture_id,capture_digest=job.capture_digest,
        currency=job.currency,requested_as_of=stamp,captured_at=stamp,inputs=job.inputs)
    @test Worker.evaluate(bad_convention).failure == F.CURVE_CALIBRATION_FAILURE_UNSUPPORTED_CONVENTION
    @test_throws Exception Worker.run(IOBuffer(),IOBuffer())
    @test_throws Exception Worker.run(IOBuffer(fill(UInt8(0),Worker.MAX_BYTES+1)),IOBuffer())
    @test_throws Exception Worker.run(IOBuffer(UInt8[0x0a,0x7f]),IOBuffer())
    missing = R.CalibrationInput(instrument_id="missing-2y",kind=R.RateQuoteKind.RATE_QUOTE_KIND_SWAP,tenor_years=2.0)
    coverage = Worker.research.domain.v1.CalibrationCoverage(configured=UInt32(2),quoted=UInt32(1),
        missing=[Worker.research.domain.v1.MissingQuote(instrument_id="missing-2y",reason="no_price")])
    partial = R.CurveCalibrationJob(capture_id=job.capture_id,capture_digest=job.capture_digest,
        currency=job.currency,requested_as_of=stamp,captured_at=stamp,interpolation=job.interpolation,
        inputs=[input,missing],coverage=coverage,latest_observations=true)
    before = IOBuffer(); encode(ProtoEncoder(before),partial); captured = take!(before)
    candidate = Worker.evaluate(partial)
    @test candidate.failure == F.CURVE_CALIBRATION_FAILURE_NONE
    @test length(candidate.pillars) == 1
    @test candidate.capture_digest == partial.capture_digest
    after = IOBuffer(); encode(ProtoEncoder(after),partial)
    @test take!(after) == captured # no provenance or missing-input mutation
    # A measured zero must survive the wire while absent remains absent. Some
    # generators erase proto3 optional scalar presence; explicit oneof prevents
    # that from fabricating a zero quote for an unobserved configured input.
    zero = R.CalibrationInput(instrument_id="zero",kind=input.kind,tenor_years=1.0,
        rate=OneOf(:resolved_rate,0.0))
    for original in (zero,missing)
        io=IOBuffer(); encode(ProtoEncoder(io),original); seekstart(io)
        decoded=decode(ProtoDecoder(io),R.CalibrationInput)
        @test (decoded.rate === nothing) == (original.rate === nothing)
        original.rate === nothing || @test decoded.rate[] === 0.0
    end
end
