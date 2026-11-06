package arch

import (
	"fmt"
	"strings"
	"testing"
)

// This bounded source guard checks the named serializer/restore seams, not all
// Go control flow. Numerical and restart tests independently exercise behavior.
func checkpointPrecisionBoundary(source, schema string) error {
	for _, token := range []string{
		"cp, err := p.snapshotLocked()", "if err != nil {", "return err",
		"ExactEncodingVersion: 1", "d, ok := dec.ToProtoExact(r)",
		"d, ok := dec.ToProtoExact(r)\n\tif !ok {\n\t\treturn nil, ErrCheckpointValueOutOfDomain\n\t}",
		"if cp.GetExactEncodingVersion() != 1 {", "return ErrCheckpointEncodingVersion",
	} {
		if !strings.Contains(source, token) {
			return fmt.Errorf("checkpoint exact boundary missing %q", token)
		}
	}
	if strings.Contains(source, "dec.ToProtoScaled(") {
		return fmt.Errorf("rounded checkpoint serializer")
	}
	refusal := strings.Index(source, "if cp.GetExactEncodingVersion() != 1 {")
	mutation := strings.Index(source, "p.accounts[a.tenant][a.id] = a")
	if mutation < 0 || refusal > mutation {
		return fmt.Errorf("encoding refusal must precede restore mutation")
	}
	if !strings.Contains(schema, "uint32 exact_encoding_version = 5;") {
		return fmt.Errorf("exact encoding contract missing")
	}
	return nil
}

func TestCheckpointPrecisionBoundary(t *testing.T) {
	source, schema := readStripped(t, cpFileRel), readStripped(t, cpSchemaRel)
	if err := checkpointPrecisionBoundary(source, schema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"rounding", "dec.ToProtoExact(r)", "dec.ToProtoScaled(r)"},
		{"ignore error", "cp, err := p.snapshotLocked()", "cp, _ := p.snapshotLocked()"},
		{"missing marker", "ExactEncodingVersion: 1", "ExactEncodingVersion: 0"},
		{"accept legacy", "if cp.GetExactEncodingVersion() != 1 {", "if cp.GetExactEncodingVersion() > 1 {"},
		{"silent conversion", "return nil, ErrCheckpointValueOutOfDomain", "return nil, nil"},
		{"ignore version", "return ErrCheckpointEncodingVersion", "return nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(source, tc.before, tc.after, 1)
			if changed == source {
				t.Fatal("dead mutation")
			}
			if checkpointPrecisionBoundary(changed, schema) == nil {
				t.Fatal("mutation escaped guard")
			}
		})
	}
	if checkpointPrecisionBoundary(source, strings.Replace(schema, "uint32 exact_encoding_version = 5;", "", 1)) == nil {
		t.Fatal("schema mutation escaped guard")
	}
}
