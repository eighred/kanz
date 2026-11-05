using ProtoBuf
VERSION == v"1.13.3" || error("Julia 1.13.3 required")
root = normpath(joinpath(@__DIR__, "..", "kanz-schemas", "proto"))
mkpath(joinpath(@__DIR__, "gen"))
protojl("research/v1/curve_calibration.proto", root, joinpath(@__DIR__, "gen"); add_kwarg_constructors=true)
