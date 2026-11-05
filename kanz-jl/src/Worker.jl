module Worker
using ProtoBuf
include("CurveKernel.jl")
using .CurveKernel

# Generated exclusively from the repository's canonical protobuf imports.
include(joinpath(@__DIR__, "..", "gen", "research", "research.jl"))
const R = research.v1
const F = R.CurveCalibrationFailure
const MAX_BYTES = 1 << 20
validtime(t) = t !== nothing && -62135596800 <= t.seconds <= 253402300799 && 0 <= t.nanos < 1000000000

function result(job, failure; pillars=R.CurveCalibrationPillar[])
    R.CurveCalibrationResult(job.capture_id, job.capture_digest, pillars, failure)
end

function evaluate(job::R.CurveCalibrationJob)
    if isempty(job.capture_id) || ncodeunits(job.capture_id) > 128 || length(job.capture_digest) != 32 ||
       !occursin(r"^[A-Z]{3}$", job.currency) || !validtime(job.requested_as_of) ||
       !validtime(job.captured_at) || !(1 <= length(job.inputs) <= 256) ||
       any(q -> isempty(q.instrument_id) || ncodeunits(q.instrument_id) > 256 ||
           (q.observation !== nothing && q.observation.instrument_id != q.instrument_id), job.inputs)
        return result(job, F.CURVE_CALIBRATION_FAILURE_INVALID_INPUT)
    end
    if Int(job.interpolation) != 1
        return result(job, F.CURVE_CALIBRATION_FAILURE_UNSUPPORTED_CONVENTION)
    end
    quotes = [(kind=Int(q.kind), tenor=q.tenor_years, span=q.span_years, rate=q.rate[])
              for q in job.inputs if q.rate !== nothing]
    try
        tenors, zeros = calibrate(quotes)
        pillars = [R.CurveCalibrationPillar(t,z) for (t,z) in zip(tenors,zeros)]
        result(job, F.CURVE_CALIBRATION_FAILURE_NONE; pillars)
    catch err
        err isa InvalidInput && return result(job, F.CURVE_CALIBRATION_FAILURE_INVALID_INPUT)
        err isa NumericalFailure && return result(job, F.CURVE_CALIBRATION_FAILURE_NUMERICAL_FAILURE)
        rethrow()
    end
end

function run(input::IO, output::IO)
    bytes = read(input, MAX_BYTES + 1)
    0 < length(bytes) <= MAX_BYTES || error("invalid input size")
    job = decode(ProtoDecoder(IOBuffer(bytes)), R.CurveCalibrationJob)
    candidate = evaluate(job)
    buffer = IOBuffer()
    encode(ProtoEncoder(buffer), candidate)
    position(buffer) <= MAX_BYTES || error("invalid output size")
    write(output, take!(buffer))
    flush(output)
    nothing
end
end
