package copilot

import "github.com/eighred/kanz/internal/tui/pane"

// Compile-time: silently losing TextInput would not break the build, it would
// just stop the mode reaching the pane — so assert it.
var _ pane.TextInput = (*Pane)(nil)
