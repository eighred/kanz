package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE kanz SHELL MUST NOT LINK THE BUS.
//
// kanz replaces four terminal binaries that span two auth planes (#65). The
// gateway-plane tools (kanz, universe) authenticate as a HUMAN with an SSO
// device-flow token. The bus-plane tools (kanz-halt, kanz-monitor, and the three
// event CLIs) each hold their OWN SPIFFE SVID, with their own NetworkPolicy and
// their own NATS publish grant in infra/nats/tenancy.yaml.
//
// The shell reaches the bus tools by EXECUTING them — pane.ExecPane, run through
// tea.ExecProcess — so each keeps its identity and the shell needs none. That is
// the entire reason the fold is safe.
//
// It stops being safe the moment the shell links pkg/bus or pkg/transport. Then
// kanz needs an SVID of its own, and there is exactly one SVID for a binary that
// hosts both the read-only monitor and the kill switch: one that can do both.
// That collapses five least-privilege identities into one and discards
// cmd/kanz-halt's stated reason for existing — "the one tool that must work
// while the system is on fire cannot be coupled to the system that is on fire".
//
// This guard is what makes the split enforced rather than merely intended.
// Nothing else would notice: importing pkg/bus compiles, passes every test, and
// looks like progress on #67.
func TestKanzShellDoesNotLinkTheBus(t *testing.T) {
	root := moduleRoot(t)

	// Packages that make up the shell. cmd/kanz is the binary; internal/tui is
	// everything it is built from, and is scanned too because an import added
	// there reaches the shell just as surely.
	shellTrees := []string{
		filepath.Join(root, "cmd", "kanz"),
		filepath.Join(root, "internal", "tui"),
	}

	// The bus is these two packages. Named explicitly rather than pattern-matched
	// on "bus": a denylist of substrings would miss a rename and would flag
	// unrelated packages that merely have the word in them.
	forbidden := map[string]string{
		"github.com/eighred/kanz/pkg/bus": "the NATS client — linking it means this binary talks to " +
			"the spine, which requires an SVID it must not have",
		"github.com/eighred/kanz/pkg/transport": "the SPIFFE mTLS transport — its only purpose is " +
			"presenting a workload identity to the broker",
	}

	var problems []string
	scanned := 0

	for _, tree := range shellTrees {
		err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Test files are excluded: a test may legitimately construct a bus
			// fake, and a test binary is not the shipped shell.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++

			// PARSED, NOT GREPPED. An import path inside a comment or a string
			// literal is not an import, and this guard's whole subject is the
			// difference between mentioning something and linking it — the same
			// distinction serviceMentions had to learn twice (#147).
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			rel, _ := filepath.Rel(root, path)
			for _, spec := range f.Imports {
				imp, uerr := strconv.Unquote(spec.Path.Value)
				if uerr != nil {
					continue
				}
				if reason, bad := forbidden[imp]; bad {
					problems = append(problems, fmt.Sprintf(
						"%s imports %s\n\n"+
							"      %s.\n\n"+
							"      The kanz shell reaches bus-plane tools by EXECUTING them "+
							"(internal/tui/pane.ExecPane via tea.ExecProcess), so kanz-monitor keeps its "+
							"read-only SVID and kanz-halt keeps its own. Linking the bus here would need "+
							"one identity that is both, which is the collapse #65 and #67 exist to "+
							"prevent. Reach the tool as a child process instead.",
						filepath.ToSlash(rel), imp, reason))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}

	// NON-VACUITY. A walk that finds no files passes no matter what the shell
	// imports — a broken scanner reads exactly like a clean estate, which is the
	// failure this repository keeps paying for.
	if scanned == 0 {
		t.Fatal("scanned zero non-test .go files across cmd/kanz and internal/tui — the shell moved or " +
			"the scanner is broken, not the estate")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("the kanz shell links the bus:\n\n  %s", strings.Join(problems, "\n\n  "))
	}
	t.Logf("%d shell source file(s) checked; none link pkg/bus or pkg/transport", scanned)
}
