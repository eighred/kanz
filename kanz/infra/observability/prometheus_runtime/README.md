# Prometheus runtime qualification (#1140)

The Dockerfile copies the pinned upstream Prometheus binaries and notices into
the approved distroless-static image. The upstream image must first exist in
the private base-image mirror at the exact source digest (#1576).

`prometheus-runtime.yml` builds and qualifies the image on Linux. Its receipt
binds the source/tree, recipe, binary hashes and resolved image identities. The
upstream shell inventory is the affected control; the candidate must contain
none of those utilities. Both images run non-root with read-only roots, no
capabilities, bounded resources and only an isolated loopback network. A dummy
TLS pod API exercises discovery, and the same owned, size-bounded memory volume
survives a process restart to check retained queries. This does not establish
production PVC continuity or node-loss recovery.

Streaming-list evidence requires both initial-event request and completed
bookmark-response write, with exact boolean status fields. The fixture records
success only after the complete body is written and flushed; it does not assert
a client acknowledgement. Watch state belongs to each fixture server. Both
initial startup and the recreated fixture after Prometheus restart must satisfy
this control before a successful runtime receipt can be returned.

`test_watch_protocol.py` covers malformed/incomplete status, per-server state,
failed/partial writes, restart enforcement and retained-history refusal. Run it
only inside the reviewed Linux sandbox. Its HTTP cases exercise a disposable
loopback fixture; Docker/Prometheus calls in control-flow tests are mocked.
These tests do not qualify the actual Prometheus image, TLS client, native
process profile or release path. The bounded-supervisor and observed runtime
profile work recorded in #1578 remains required before release acceptance.

The supervisor's command scope supplies an explicit safe environment with
private scratch-local home, temporary, cache and Docker configuration paths.
Each command shares a 1MiB stdout/stderr limit and a deadline of at most 60s;
owned process-group cleanup precedes reaping its leader. `test_command.py`
exercises these controls with local disposable Python processes, including a
pipe-holding descendant. Aggregate qualification/cleanup deadlines and actual
Docker image/process-profile acceptance remain required under #1578.

The workflow has read-only package access and publishes only a non-secret test
receipt. Build/release matrix integration, final signed ECR image/scan evidence
and an authorized Tokyo deployment preserving the existing PVC remain on
#1140. Adding the image to the fixed build inventory requires the corresponding
architecture guard update and its full applicable qualification; #1564 currently
blocks that Julia advisory gate. Preserve the gate.
