// Package mode holds the wire contract for the platform halt signal: the subject
// the lifecycle.v1.ModeChanged FACT travels on, and the component name that marks
// a whole-system transition.
//
// It exists as its own package, holding nothing but constants and importing
// nothing, so that the two ends of the kill-switch can share one definition
// WITHOUT sharing a dependency:
//
//   - internal/signal/translate consumes the FACT (the Gate both brains check).
//   - cmd/kanz-halt publishes it (the operator's break-glass handle).
//
// If the publisher instead imported the translator, the break-glass tool would
// only build when the entire trading pipeline builds — the one binary that must
// work during an incident would be coupled to the code causing it. And if the two
// sides each declared their own copy of the subject string, a rename on one side
// would leave a kill-switch that publishes into the void and a gate that never
// hears it. Neither failure is acceptable for a brake, so the contract lives here.
package mode

// Subject carries the lifecycle.v1.ModeChanged FACT — the halt signal.
const Subject = "platform.mode.changed"

// ComponentSystem is the ModeChanged.component value for a whole-system
// transition, as opposed to a single component's. The Gate reacts to this and
// nothing else.
const ComponentSystem = "system"
