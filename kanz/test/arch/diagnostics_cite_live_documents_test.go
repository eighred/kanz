package arch

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A diagnostic must not direct a repair toward a document the checkout cannot
// provide (#1327). Historical quotes remain valid when they do not tell a
// reader to act on the deleted document.
var actionableDocument = regexp.MustCompile(`(?i)\b(?:read|amend|update|consult|open|follow|see)\s+(?:the\s+)?([A-Za-z0-9_./\\-]+\.md)`)

func missingActionableDocument(text string, docs map[string]bool) string {
	for _, match := range actionableDocument.FindAllStringSubmatch(text, -1) {
		name := match[1]
		if !docs[name] && !docs[filepath.Base(name)] {
			return name
		}
	}
	return ""
}

func TestDiagnosticDocumentReferencesAreActionable(t *testing.T) {
	missing := "missing-instructions.md"
	for _, verb := range []string{"read ", "amend ", "see "} {
		if got := missingActionableDocument(verb+missing, map[string]bool{}); got != missing {
			t.Fatalf("%s diagnostic target = %q, want %q", verb, got, missing)
		}
	}
	if got := missingActionableDocument("historical decision recorded in "+missing, map[string]bool{}); got != "" {
		t.Fatalf("dated provenance was treated as an instruction: %q", got)
	}
}

func TestNoDiagnosticDirectsAnEngineerToAMissingDocument(t *testing.T) {
	root := moduleRoot(t)
	docs := markdownIndex(t, filepath.Dir(root))
	var bad []string
	stringsSeen := 0
	files := walkGoProse(t, root, func(site proseSite) {
		if site.Kind != proseString {
			return
		}
		stringsSeen++
		if name := missingActionableDocument(site.Text, docs); name != "" {
			bad = append(bad, site.File+": "+name)
		}
	})
	if files < 500 || stringsSeen < 1000 {
		t.Fatalf("document diagnostic scan was vacuous: %d files, %d strings", files, stringsSeen)
	}
	if len(bad) > 0 {
		t.Fatalf("diagnostics direct engineers to absent documents:\n%s", strings.Join(bad, "\n"))
	}
}
