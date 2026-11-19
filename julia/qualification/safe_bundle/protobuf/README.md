# Protobuf source for the compiled calibration worker

The bounded Julia worker reads one request into an `IOBuffer`. Its generated decoder must fold repeated occurrences of a singular embedded message while preserving wire presence. This includes message-valued oneof members. Later occurrences of the same member merge. Switching members discards the previous value. ProtoBuf.jl 1.3.0's post-decode struct merge loses this information and cannot resolve the calibration worker's market-data oneof under safe native trimming.

This qualification materializes a worker-specific adaptation of [ProtoBuf.jl 1.3.0](https://github.com/JuliaIO/ProtoBuf.jl/tree/b3d83a7ce236ce44d8de6e688080069f176f4884). The local package has its own source identity. The patch retains each embedded wire boundary and uses the generated field loop across its fragments. It also parses discarded oneof values before resetting them. Canonical schemas and the generated SDK types remain the contract. This profile supports the worker's bounded in-memory proto3 input. Other stream types and general proto2 compatibility are outside its qualification.

## Materialize and select

Use a fresh isolated copy of the repository with the pinned Julia toolchain and dependency depot. Pin the upstream checkout to the revision above. From the repository root on Linux, use a new writable scratch directory for the package and fixture outputs.

```sh
profile=$PWD/julia/qualification/safe_bundle/protobuf
python3 -B "$profile/materialize.py" "$upstream" "$scratch/ProtoBuf" > "$scratch/source.json"
cp julia/Manifest.toml "$scratch/registered.toml"
julia --startup-file=no "$profile/select_project.jl" \
  "$scratch/registered.toml" "$scratch/ProtoBuf" "$scratch/selected.toml"
cp "$scratch/selected.toml" julia/Manifest.toml
julia --startup-file=no --project=julia julia/generate.jl
julia --startup-file=no --project=julia julia/test/runtests.jl
```

Keep the path manifest confined to that disposable project. The materializer checks the registered source tree, patch SHA-256 and applied Git tree. The selector checks the actual checkout before it selects the path. The upstream [MIT license](LICENSE.md) accompanies the adaptation.

## Compare generated bindings against Go

Generate the canonical Go SDK as described in [schemas](../../../../schemas/README.md). Run the two Go commands from the Kanz module. Each fixture checks its constructed semantic expectation against the Go decoder. The Julia process independently decodes and re-encodes each input. Go reads those bytes back and checks message presence, nested decimals, repeated fields and explicit defaults.

```sh
mkdir "$scratch/wire"
(cd kanz && go run "$profile/wire_oracle.go" write "$scratch/wire")
julia --startup-file=no --project=julia "$profile/roundtrip.jl" julia "$scratch/wire" "$scratch/wire"
(cd kanz && go run "$profile/wire_oracle.go" check "$scratch/wire")
```

Native acceptance also requires JuliaC's safe trim verifier, the final executable's Go calibration corpus, and the shipped OCI and advisory checks. Source materialization alone cannot certify a released worker.
