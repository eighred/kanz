package ledger

import (
	"fmt"
	"testing"
)

// EntryTypes and entryTypeNames are what the accounting composition root
// enumerates to seed one posture series per entry type (#588). A type declared in
// the iota block but missing from either would ship with NO SERIES AT ALL until
// its first posting — absent rather than zero, which is the exact failure the
// posture exists to remove (liquidity.go shipped a hand-enumerated reason set
// with that hole). entryTypeCount is the sentinel that makes forgetting loud.
func TestEntryTypesCoversEveryDeclaredType(t *testing.T) {
	if len(EntryTypes) != int(entryTypeCount) {
		t.Fatalf("EntryTypes has %d entries but %d entry types are declared — a new type would "+
			"be seeded into no metric series at all; extend EntryTypes", len(EntryTypes), entryTypeCount)
	}
	if len(entryTypeNames) != int(entryTypeCount) {
		t.Fatalf("entryTypeNames has %d entries but %d entry types are declared — an unnamed type "+
			"renders as entry_type(N) in a metric label", len(entryTypeNames), entryTypeCount)
	}
	for i, typ := range EntryTypes {
		if int(typ) != i {
			t.Fatalf("EntryTypes[%d] = %d: the slice must be in declaration order so the index is the value", i, typ)
		}
	}
}

// A blank label value is indistinguishable from an unset one, so no name may be
// empty and no two may collide (two types folding into one series hides one).
func TestEntryTypeNamesAreUniqueAndNonEmpty(t *testing.T) {
	seen := map[string]EntryType{}
	for _, typ := range EntryTypes {
		name := typ.String()
		if name == "" {
			t.Fatalf("entry type %d has an empty name", typ)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("entry types %d and %d both render as %q", prev, typ, name)
		}
		seen[name] = typ
	}
	if got := EntryCorporateAction.String(); got != "corporate_action" {
		t.Fatalf("EntryCorporateAction.String() = %q, want corporate_action (nav.go's attribution "+
			"component and any PromQL naming it are frozen on this spelling)", got)
	}
}

// An out-of-range value must not render blank: it is reached by a decoder that
// read a type this build does not know, and a blank label would hide it.
func TestUndeclaredEntryTypeRendersVisibly(t *testing.T) {
	undeclared := EntryType(entryTypeCount + 7)
	want := fmt.Sprintf("entry_type(%d)", int(undeclared))
	if got := undeclared.String(); got != want {
		t.Fatalf("undeclared entry type rendered as %q, want %q", got, want)
	}
	if got := EntryType(-3).String(); got != "entry_type(-3)" {
		t.Fatalf("negative entry type rendered as %q, want entry_type(-3)", got)
	}
}
