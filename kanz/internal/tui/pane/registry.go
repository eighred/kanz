package pane

import "fmt"

// Registry is the shell's ordered set of panes: the tab order the operator
// sees, and the lookup the router uses.
//
// ORDER IS DATA, NOT INCIDENTAL. Tab order is what an operator's fingers learn,
// so it is fixed by the order panes are added rather than by map iteration —
// a shell whose tabs move between runs is one nobody can drive without looking.
type Registry struct {
	order []ID
	byID  map[ID]Pane
}

// NewRegistry builds a registry from panes in tab order.
//
// It returns an error rather than panicking on a duplicate id: the registry is
// assembled at startup from wiring code, and a duplicate is a wiring bug the
// composition root should report with everything else, not a crash in a library.
func NewRegistry(panes ...Pane) (*Registry, error) {
	r := &Registry{byID: make(map[ID]Pane, len(panes))}
	for _, p := range panes {
		if p == nil {
			return nil, fmt.Errorf("pane: nil pane in registry at position %d", len(r.order))
		}
		id := p.ID()
		if id == "" {
			return nil, fmt.Errorf("pane: pane %q has an empty id — it could not be addressed or bound to a key", p.Title())
		}
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("pane: duplicate pane id %q — ids address panes, so two panes cannot share one", id)
		}
		// A Bus pane that cannot say what to run has no way to be reached, and
		// the shell must never fall back to running it in-process (see the
		// package doc). Catch it here, at wiring time, not on first keypress.
		if p.Plane() == Bus {
			if _, ok := p.(Runner); !ok {
				return nil, fmt.Errorf("pane: %q is on the bus plane but does not implement Runner — "+
					"a bus pane runs as a child process holding its own SVID, so it must supply a command", id)
			}
		}
		r.order = append(r.order, id)
		r.byID[id] = p
	}
	if len(r.order) == 0 {
		return nil, fmt.Errorf("pane: registry is empty — a shell with no panes has nothing to route to")
	}
	return r, nil
}

// Len is the number of panes.
func (r *Registry) Len() int { return len(r.order) }

// At returns the pane at tab index i, and whether i was in range.
func (r *Registry) At(i int) (Pane, bool) {
	if i < 0 || i >= len(r.order) {
		return nil, false
	}
	return r.byID[r.order[i]], true
}

// IndexOf returns the tab index of id, or -1.
func (r *Registry) IndexOf(id ID) int {
	for i, got := range r.order {
		if got == id {
			return i
		}
	}
	return -1
}

// IDs returns the pane ids in tab order.
func (r *Registry) IDs() []ID {
	out := make([]ID, len(r.order))
	copy(out, r.order)
	return out
}

// Replace swaps the pane at index i, which is how the router stores the value a
// pane's Update returned.
func (r *Registry) Replace(i int, p Pane) bool {
	if i < 0 || i >= len(r.order) || p == nil {
		return false
	}
	if p.ID() != r.order[i] {
		// An Update that changed its own id would silently detach the pane from
		// its tab and its key binding.
		return false
	}
	r.byID[r.order[i]] = p
	return true
}
