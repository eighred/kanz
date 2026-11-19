@assert VERSION == v"1.13.3"
@assert Set(pkg.name for pkg in Base._sysimage_modules) ==
    Set(["Base", "Core", "Main", "KanzCurveCandidate", "ProtoBuf", "EnumX", "Unicode", "Dates", "Printf", "BufferedStreams", "TOML"])
@assert ccall(:geteuid, Cuint, ()) == 65532
@assert readdir("/sys/class/net") == ["lo"]
@assert strip(read("/sys/fs/cgroup/memory.max", String)) == "1073741824"
@assert strip(read("/sys/fs/cgroup/pids.max", String)) == "64"
@assert split(read("/sys/fs/cgroup/cpu.max", String)) == ["100000", "100000"]
status = read("/proc/self/status", String)
@assert occursin(r"(?m)^CapEff:\s+0000000000000000$", status)
@assert occursin(r"(?m)^NoNewPrivs:\s+1$", status)
@assert ENV["JULIA_PKG_OFFLINE"] == "true"
pcre_version = zeros(UInt8, 64)
@assert ccall((:pcre2_config_8, "libpcre2-8"), Cint, (Cuint, Ptr{UInt8}), 11, pcre_version) > 0
@assert startswith(unsafe_string(pointer(pcre_version)), "10.49 ")
@assert occursin(r"^a[0-9]+$", "a49")
# Distinguish the statically linked copy in Julia core from the standalone
# Julia-patched libuv shared object. Both report the same development version.
function native_version(file, symbol)
    path = realpath(joinpath("/opt/kanz/lib/julia", file))
    handle = ccall(:dlopen, Ptr{Cvoid}, (Cstring, Cint), path, 1)
    @assert handle != C_NULL
    function_pointer = ccall(:dlsym, Ptr{Cvoid}, (Ptr{Cvoid}, Cstring), handle, symbol)
    @assert function_pointer != C_NULL
    owner = Ref(ntuple(_ -> Ptr{Cvoid}(0), 4))
    @assert ccall(:dladdr, Cint, (Ptr{Cvoid}, Ref{NTuple{4,Ptr{Cvoid}}}), function_pointer, owner) != 0
    @assert realpath(unsafe_string(Ptr{UInt8}(owner[][1]))) == path
    return unsafe_string(ccall(function_pointer, Cstring, ()))
end

core = "libjulia-internal.so.1.13.3"
@assert native_version(core, "utf8proc_version") == "2.11.3"
@assert native_version(core, "utf8proc_unicode_version") == "17.0.0"
@assert native_version(core, "uv_version_string") == "2.0.0-dev"
@assert native_version("libuv.so.2.0.0", "uv_version_string") == "2.0.0-dev"

# Test the kernel boundary directly. Julia's buffered write can return a short
# count while dropping the underlying ENOSPC.
buffer = zeros(UInt8, 1 << 20)
open("/tmp/bounded", "w") do io
    for _ in 1:16
        n = ccall(:write, Cssize_t, (Cint, Ptr{UInt8}, Csize_t), fd(io), buffer, length(buffer))
        @assert n == length(buffer)
    end
    n = ccall(:write, Cssize_t, (Cint, Ptr{UInt8}, Csize_t), fd(io), buffer, 1)
    reason = Libc.errno()
    @assert n == -1 && reason == Libc.ENOSPC
end
@assert filesize("/tmp/bounded") == 16 << 20
println("ENOSPC at 16777216 bytes")
