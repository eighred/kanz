using ProtoBuf

length(ARGS) == 3 || error("usage: roundtrip.jl JULIA_PROJECT INPUT_DIR OUTPUT_DIR")
include(joinpath(abspath(ARGS[1]), "src", "Worker.jl"))

# Observe real decoding in this qualification process. The Go result alone
# cannot distinguish a fully read prior trade from a silently discarded one.
const trade_reads = String[]
const Trade = Worker.research.market.v1.Trade
function Base.getindex(buffer::ProtoBuf.Codecs.MessageFragments{Trade})
    value = invoke(Base.getindex, Tuple{ProtoBuf.Codecs.MessageFragments}, buffer)
    isnothing(value) || push!(trade_reads, value.trade_id)
    return value
end

for name in sort(readdir(ARGS[2]))
    endswith(name, ".pb") || continue
    wire = read(joinpath(ARGS[2], name))
    empty!(trade_reads)
    message = decode(ProtoDecoder(IOBuffer(wire)), Worker.R.CalibrationInput)
    if name == "switch.pb"
        trade_reads == ["first"] || error("discarded trade was not consumed once")
    elseif name == "switch_back.pb"
        trade_reads == ["first", ""] || error("trade cases did not decode independently")
    end
    open(joinpath(ARGS[3], first(splitext(name)) * ".decoded"), "w") do io
        encode(ProtoEncoder(io), message)
    end
end
