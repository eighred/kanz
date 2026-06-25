# autopilot service (AUTO-01)

The closed-loop ops controller. The signals already exist — DATA-07 quality
events, DATA-05 reconciliation divergence, model drift, SLO burn-rate, inference
circuit-breaker state — but nothing acted on them. autopilot adds the actuation
layer: watch the signal stream, match each signal to a recognized condition, run
that condition's remediation runbook, and escalate to a human on anything novel.

## Control loop

```
bus events ─▶ classify ─▶ match condition ─▶ run runbook ─▶ remediate / actuate
 (signals)   (internal/    (plan.Matcher)    (runbook-as-     (quarantine, model
              signal)                          code)            rollback, scale,
                                                                failover)
                            │ no match / step failed
                            ▼
                       escalate to human (AUTO-01d)
```

| Task | Package | What |
|---|---|---|
| AUTO-01a | `internal/controller`, `internal/signal` | event-driven control loop + signal normalization |
| AUTO-01b | `internal/remediate` | data quarantine + model auto-rollback (behind seams) |
| AUTO-01c | `internal/actuate` | scale-out + regional failover (behind seams), closing the INFRA-01c/DR-01d loop |
| AUTO-01d | `internal/runbook`, `internal/plan`, `kanz/docs/runbooks/autopilot.md` | runbook-as-code + human-in-the-loop escalation |

## Conservative by design

- **Severity floors**: only CRITICAL data/drift/staleness/SLO conditions
  auto-remediate; softer signals escalate (`internal/plan`).
- **Failover is gated**: the hardest-to-reverse action (DR-01d regional failover)
  is off by default — a sustained critical SLO burn auto-scales and escalates the
  failover to a human. Enable with `AUTOPILOT_AUTO_FAILOVER=true`.
- **Fail to a human, not blindly**: a remediation step that errors stops the
  runbook and escalates rather than retrying.
- **Seams, not hardcoded clients**: remediators/actuators are interfaces with
  log-by-default impls (a real deployment wires command-publishers / a k8s
  client) — the repo's DEBT-02 "inject the side-effect" stance.

## Run

```sh
AUTOPILOT_NATS_URL=nats://localhost:4222 go run ./services/autopilot/cmd/autopilot
```

Config (env): `AUTOPILOT_LISTEN` (`:8087`), `AUTOPILOT_NATS_URL` (unset ⇒ probes
only), `AUTOPILOT_SUBJECTS` (default `observation.> platform.> data.>`),
`AUTOPILOT_AUTO_FAILOVER` (default off), `AUTOPILOT_CONSUMER_GROUP`,
`AUTOPILOT_SOURCE`, `AUTOPILOT_OTLP_ENDPOINT`.

## Tests (AUTO-01e)

`go test ./services/autopilot/...` — a simulated broker stall (staleness /
circuit-open), model drift, and data gap are each auto-remediated (quarantine /
rollback / scale), and escalation fires only on unrecognized conditions
(sub-threshold WARNING, or a failed remediation step); plus signal classification
(DataQualityEvent kinds + event-type conditions) and the failover gating.
