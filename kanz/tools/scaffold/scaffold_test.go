package scaffold_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/tools/scaffold"
)

func TestGenerateProducesValidOnConventionService(t *testing.T) {
	root := t.TempDir()
	created, err := scaffold.Generate(root, "widget-svc")
	if err != nil {
		t.Fatal(err)
	}

	// EVT-16a layout: cmd/<name>/main.go + per-service internal packages.
	wantRel := []string{
		"services/widget-svc/cmd/widget-svc/main.go",
		"services/widget-svc/internal/config/config.go",
		"services/widget-svc/internal/server/server.go",
		"services/widget-svc/README.md",
	}
	if len(created) != len(wantRel) {
		t.Fatalf("created %v, want %d files", created, len(wantRel))
	}
	for _, rel := range wantRel {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("expected %s: %v", rel, err)
		}
	}

	// Every generated .go file must parse — the scaffold gofmt's on write, so a
	// broken template fails generation, but re-parse here as the contract.
	fset := token.NewFileSet()
	for _, rel := range wantRel {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		if _, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.AllErrors); err != nil {
			t.Errorf("generated %s does not parse: %v", rel, err)
		}
	}

	// Name substitution reached the env prefix + import paths.
	main, _ := os.ReadFile(filepath.Join(root, "services/widget-svc/cmd/widget-svc/main.go"))
	if !strings.Contains(string(main), "github.com/kanz-eng/kanz/services/widget-svc/internal/config") {
		t.Error("main.go missing the service's internal import path")
	}
	cfg, _ := os.ReadFile(filepath.Join(root, "services/widget-svc/internal/config/config.go"))
	if !strings.Contains(string(cfg), "WIDGET_SVC_LISTEN") {
		t.Error("config.go missing the derived env prefix WIDGET_SVC_")
	}
}

func TestGenerateRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold.Generate(root, "dup"); err != nil {
		t.Fatal(err)
	}
	_, err := scaffold.Generate(root, "dup")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second Generate = %v, want overwrite refusal", err)
	}
}

func TestGenerateRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"", "A", "Foo", "bad name", "-leading", "x", "with_underscore", "trailing-"} {
		if _, err := scaffold.Generate(root, bad); err == nil {
			t.Errorf("Generate(%q) succeeded, want rejection", bad)
		}
	}
}
