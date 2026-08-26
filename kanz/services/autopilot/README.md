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
| AUTO-01d | `internal/runbook`, `internal/plan`, the runbook below | runbook-as-code + human-in-the-loop escalation |

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

## autopilot autonomous operations (AUTO-01)

Severity **page** · alert `AutopilotEscalation` · SLO `autopilot/control-loop`.


The `autopilot` service is the closed-loop ops controller: it watches the
operational signal stream and runs **runbook-as-code** — the human runbooks in
this directory, codified. This page is the human-readable index of what it does
autonomously and when it hands back to you.

### Codified runbooks (AUTO-01a–c)

The control policy lives in `services/autopilot/internal/plan`. Each recognized
condition maps to an ordered remediation runbook:

| Condition (signal → severity floor) | Runbook | Task |
|---|---|---|
| `data_gap` ≥ critical | quarantine the data subject | AUTO-01b |
| `reconcile_divergence` ≥ warning | quarantine the divergent subject | AUTO-01b |
| `drift` ≥ critical | roll the model back (MLOPS-01e in reverse) | AUTO-01b |
| `staleness` ≥ critical | scale out `market-data` ingest | AUTO-01c |
| `circuit_open` ≥ warning | scale out `inference` | AUTO-01c |
| `slo_burn` ≥ critical | scale out `risk-engine` (+ failover if enabled) | AUTO-01c |

Signals are the DATA-07 quality events (gap/staleness/drift), DATA-05
reconciliation divergence, model drift, OBS-01 SLO fast-burn, and the PRED-07
inference circuit-breaker — normalized in `internal/signal`.

### When autopilot escalates to you (AUTO-01d)

Escalation (`AutopilotEscalation` page) fires when the loop will NOT act
autonomously, so a human decides:

- **Unrecognized / sub-threshold condition** — a signal that matches no runbook,
  e.g. a WARNING data gap below the auto-quarantine floor. Autopilot deliberately
  does not act on soft signals.
- **A remediation step failed** — the quarantine/rollback/scale call errored.
  Autopilot stops the runbook and pages rather than retrying blindly.
- **Regional failover** — the hardest-to-reverse action. With
  `AUTOPILOT_AUTO_FAILOVER` off (the default), a sustained critical SLO burn only
  auto-scales and escalates the failover decision to you; turn it on only for
  environments where autonomous DR-01d failover is acceptable.

### Confirm

```promql
# What has autopilot done recently, remediated vs escalated?
sum by (condition, outcome) (rate(kanz_autopilot_outcomes_total[15m]))
```

A rising `outcome="escalated"` with no matching `remediated` means a condition is
recurring that autopilot can't fix — treat it as the real incident and use that
condition's own runbook in this directory.

### Mitigate / Recover

Autopilot's actions are the mitigations. To clear a quarantine or undo a model
rollback once the root cause is fixed, use the data-quality / model-promotion
runbooks; autopilot does not auto-un-quarantine (re-admitting bad data is a human
decision). To pause autopilot, scale its Deployment to zero — the platform keeps
running, only the autonomous remediation stops.

### Root-cause pointers

The escalation log line (`autopilot human escalation`) carries the signal kind,
subject, severity, and reason. Cross-reference the triggering `event_id` in the
audit log (AUDIT-01) and its lineage (LIN-01) to find what produced the bad
signal.
