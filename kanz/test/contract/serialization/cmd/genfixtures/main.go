// Command genfixtures regenerates the cross-language wire fixtures
// consumed by EVT-21b's Go / Python / TS contract tests. CI runs this
// once before invoking the test suites in each language; on local dev,
// run it after editing fixtures.go (or after a kanz-schemas change that
// affects envelope.proto).
//
// Usage:
//
//	go run ./test/contract/serialization/cmd/genfixtures \
//	    -out ./test/contract/serialization/fixtures
//
// Output:
//
//	<out>/<name>.bin       — raw EventFrame wire bytes per fixture
//	<out>/manifest.json    — language-agnostic field-by-field assertions
//	                         the Python + TS tests load
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kanz-eng/kanz/test/contract/serialization"
)

func main() {
	out := flag.String("out", "", "output directory (required)")
	flag.Parse()
	if *out == "" {
		log.Fatal("genfixtures: -out is required")
	}
	if err := run(*out); err != nil {
		log.Fatalf("genfixtures: %v", err)
	}
}

func run(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", out, err)
	}
	m, err := serialization.BuildAll()
	if err != nil {
		return err
	}
	for _, f := range m.Fixtures {
		p := filepath.Join(out, f.File)
		if err := os.WriteFile(p, f.WireBytes, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		fmt.Printf("wrote %s (%d bytes)\n", p, len(f.WireBytes))
	}
	manifestPath := filepath.Join(out, "manifest.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest marshal: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}
	fmt.Printf("wrote %s\n", manifestPath)
	return nil
}
