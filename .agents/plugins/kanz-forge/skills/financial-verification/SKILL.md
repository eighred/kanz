---
name: financial-verification
description: Select evidence for Kanz pricing, risk and execution methods, then verify their implementation and governed decision path with independent numerical checks.
---

# Build and verify institutional behavior

Use financial knowledge to implement a reliable Kanz capability: pricing an
instrument, constraining risk, executing an approved intent or recording its
outcome. The deliverable is a justified code/design decision and its proof, not
an investment recommendation or a financial-analysis report. Keep production
computation in its existing Go/service owner and execution on the typed admission
path defined by AGENTS.md.

## Turn the requirement into an implementable method

Start with the behavior, actor, timing and failure consequence. Follow the
[design evidence guide](../engineering-standard/references/design-evidence.md)
to distinguish an absent algorithm from missing terms, wiring, data or authority.
Use attributable public methodology or an approved owner-supplied specification;
a competitor's advertised capability does not reveal its proprietary algorithm.
Do not add a finance repository or a parallel calculation engine to Forge.

Compare only plausible methods for this requirement: valid instrument/domain,
required inputs, calibration and stability, precision, computational cost,
determinism, and the failure behavior the consumer needs. A more sophisticated
method is not an improvement if its inputs cannot be established or it cannot
meet the owning path's latency/recovery contract. State why the existing method
can be reused, must be extended, or fails the required behavior.

Specify typed inputs/outputs, units, sign, rounding, valuation/event and knowledge
time, source revision, coverage and error semantics before implementation. Read
only the affected [domain recipe](references/domain-checks.md). Trace the value
through conversion, calculation and its caller/gate/posting: a correct formula
is insufficient if the served path drops a quality flag or uses future data.

## Make the implementation falsifiable

Choose an independent oracle: a dated primary worked example, hand-derived
rational result, or separately structured implementation. Expected values must
not call the production helper or repeat its branches. Fix assumptions and
tolerance in output units before testing; justify absolute/relative error near
zero. Do not widen a tolerance or plug a residual to hide a disagreement.

Preserve canonical Decimal boundaries and exact arithmetic for money/quantities.
For existing floating-point analytics, inspect conversions, finite-value handling
and error bounds without extending float into the book. Test cases that expose
the changed behavior: boundary equality and either side, missing/stale inputs,
signs, scale/currency conversion, partial coverage and recovery where stateful.
Use conservation or monotonicity only where the chosen model guarantees it.

If the oracle disagrees, reconcile conventions, timestamps, units and rounding
before changing code or expected values. If a method/policy definition is absent,
name the missing decision; do not invent it. A critical unknown cannot permit
admission, while failure of a noncritical analytic must not stop recording a real
fill. Numerical tests cannot authorize trades or change mandates.

Run the owning-package tests and the consumer/gate/posting checks needed for the
claim, following repository dependency and serialization rules. Return the
implementation decision, exact revision, source/oracle, expected and observed
result with units/tolerance, commands, skipped tests and unproven boundaries.
Separate arithmetic, served integration and deployment evidence. For control
changes identify the approved rule version/effective interval and decision record;
a technical pass does not establish legal applicability or policy approval.
