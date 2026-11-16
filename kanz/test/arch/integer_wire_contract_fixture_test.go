package arch

import (
	"maps"
	"strings"
	"testing"
)

func TestIntegerContractPropertiesAreParsedIndividually(t *testing.T) {
	for _, source := range []string{
		`interface CustodyBreak { status: string; revision?: string; age_seconds: number }`,
		"interface CustodyBreak {\n/* revision: number */ status: string\nrevision?: string | null\nvalues: { revision: number; other: string }\n}",
	} {
		fields, err := integerTSFields(source, "CustodyBreak")
		if err != nil || !tsTypeIsString(fields["revision"]) {
			t.Fatal("same-line/nested property boundary lost", err)
		}
	}
	for _, kind := range []string{"number", "string | number", "unknown", "any", "bigint"} {
		fields, err := integerTSFields("interface CustodyBreak { status: string; revision: "+kind+"; age_seconds: number }", "CustodyBreak")
		if err != nil || tsTypeIsString(fields["revision"]) {
			t.Fatal("unsafe string alternative accepted", kind)
		}
	}
}

func TestIntegerContractUnrelatedProtoNamesDoNotChangeTheBrowserContract(t *testing.T) {
	sources := integerWireSources(t)
	// Same spelling in an unrelated message/file must neither change identity's
	// origin nor make evidence protocol version:2/count:number into strings.
	sources["schemas/proto/accounting/v1/unrelated.proto"] = `message CorporateAction { uint64 revision = 1; uint64 count = 2; }`
	path := "schemas/proto/compliance/v1/compliance.proto"
	sources[path] += "\nmessage Unrelated {\n uint64 revision = 1;\n uint64 count = 2;\n}\n"
	if problems := integerWireProblems(sources); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestIntegerContractActualUnsafeRepresentationsFail(t *testing.T) {
	baseline := integerWireSources(t)
	if problems := integerWireProblems(baseline); len(problems) != 0 {
		t.Fatal("baseline contract unresolved", problems)
	}
	for _, mutation := range []struct{ name, path, before, after, want string }{
		{"signed account", "web/src/api/users.ts", "revision: string", "revision: number", "UserAccess.revision"},
		{"signed session epoch", "web/src/api/users.ts", "session_epoch: string", "session_epoch: number", "UserAccess.session_epoch"},
		{"signed invitation", "web/src/api/invitations.ts", "revision: string", "revision: number", "InvitationSummary.revision"},
		{"signed custody same line", "web/src/api/custody.ts", "revision?: string;", "revision?: number;", "CustodyBreak.revision"},
		{"signed action evidence", "web/src/api/custody.ts", "revision: string }", "revision: number }", "ActionState.revision"},
		{"signed audit sequence", "web/src/api/evidence.ts", "seq: string;", "seq: number;", "EvidenceRecord.seq"},
		{"signed decimal", "web/src/api/decimal.ts", "coefficient?: string", "coefficient?: number", "Decimal.coefficient"},
		{"signed order history", "web/src/api/risk.ts", "unindexed?: string", "unindexed?: number", "OrdersResponse.unindexed"},
		{"unsigned mandate queue", "web/src/api/mandates.ts", "version?: string", "version?: number", "PendingMandateChange.version"},
		{"unsigned mandate document", "web/src/api/mandates.ts", "version: string", "version: number", "MandateDocument.version"},
		{"unsigned mandate proposal", "web/src/api/mandates.ts", "version: string\n  rule_count", "version: number\n  rule_count", "ProposedMandateChange.version"},
		{"signed account projection", "kanz/services/identity/internal/server/administration_json.go", "Revision     string", "Revision     int64", "Access.revision"},
		{"signed JSON tag", "kanz/services/accounting/internal/server/custody.go", `json:"revision,string"`, `json:"revision"`, "breakView.revision"},
		{"unsigned JSON map", integerComplianceSource, "jsonUint64(m.GetVersion())", "m.GetVersion()", "handlePropose"},
		{"unsigned JSON formatter", integerComplianceSource, "strconv.FormatUint(v, 10)", "v", "jsonUint64"},
		{"bounded producer replaced", "kanz/services/accounting/internal/server/custody.go", "int64(b.Age(now).Seconds())", "now.UnixNano()", "CustodyBreak.age_seconds"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			sources := maps.Clone(baseline)
			if !strings.Contains(sources[mutation.path], mutation.before) {
				t.Fatal("mutation no longer reaches its named production contract")
			}
			sources[mutation.path] = strings.Replace(sources[mutation.path], mutation.before, mutation.after, 1)
			problems := strings.Join(integerWireProblems(sources), "\n")
			if !strings.Contains(problems, mutation.want) {
				t.Fatalf("unsafe mutation escaped %s: %s", mutation.want, problems)
			}
		})
	}
}

func TestIntegerContractMissingAndNewFieldsFailClosed(t *testing.T) {
	baseline := integerWireSources(t)
	for _, path := range []string{"kanz/internal/identity/access.go", "web/src/api/invitations.ts", "schemas/proto/compliance/v1/compliance.proto"} {
		sources := maps.Clone(baseline)
		delete(sources, path)
		if len(integerWireProblems(sources)) == 0 {
			t.Fatal("missing contract silently removed coverage", path)
		}
	}
	sources := maps.Clone(baseline)
	path := "kanz/internal/identity/access.go"
	sources[path] = strings.Replace(sources[path], "type Access struct {", "type Access struct {\n FutureCounter int64 `json:\"future_counter\"`", 1)
	if !strings.Contains(strings.Join(integerWireProblems(sources), "\n"), "future_counter") {
		t.Fatal("new signed origin field silently omitted from projection/client")
	}
}

func TestIntegerContractSameLineProtoFieldsKeepTheirOwnClientProperty(t *testing.T) {
	sources := integerWireSources(t)
	proto := "schemas/proto/compliance/v1/compliance.proto"
	client := "web/src/api/mandates.ts"
	if !strings.Contains(sources[proto], "uint64 version = 4;") || !strings.Contains(sources[client], "version: string") {
		t.Fatal("same-line mutation lost its actual Mandate/DTO edge")
	}
	sources[proto] = strings.Replace(sources[proto], "uint64 version = 4;", "uint64 version = 4; uint64 future_counter = 7;", 1)
	sources[client] = strings.Replace(sources[client], "version: string", "version: string; future_counter: number", 1)
	fields, err := integerProtoFields(sources[proto], "Mandate")
	if err != nil || len(fields) != 2 || fields[0] != "version" || fields[1] != "future_counter" {
		t.Fatal("same-line direct proto fields were not parsed independently", err)
	}
	if problems := strings.Join(integerWireProblems(sources), "\n"); !strings.Contains(problems, "MandateDocument.future_counter") {
		t.Fatal("new same-line integer accepted an unsafe client property", problems)
	}
	sources[client] = strings.Replace(sources[client], "future_counter: number", "future_counter: string", 1)
	if problems := integerWireProblems(sources); len(problems) != 0 {
		t.Fatal("safe same-line representation failed its positive control", problems)
	}
}

func TestIntegerContractNestedProtoDefinitionsKeepTheirOwnMessageOwner(t *testing.T) {
	proto := "schemas/proto/compliance/v1/compliance.proto"
	for _, nested := range []string{
		"message Nested { uint64 version = 1; uint64 count = 2; }",
		"message Nested {\n uint64 version = 1;\n uint64 count = 2;\n}",
	} {
		sources := integerWireSources(t)
		if !strings.Contains(sources[proto], "uint64 version = 4;") {
			t.Fatal("nested mutation lost its actual Mandate origin")
		}
		sources[proto] = strings.Replace(sources[proto], "uint64 version = 4;", "uint64 version = 4;\n"+nested, 1)
		sources[proto] = strings.Replace(sources[proto], "message Mandate {",
			"message IntegerWireOther { message Mandate { uint64 unrelated = 1; } }\nmessage Mandate {", 1)
		if problems := integerWireProblems(sources); len(problems) != 0 {
			t.Fatal("nested definition changed its containing message's scalar contract", problems)
		}
	}
	if _, err := integerProtoFields("message Other { message Mandate { uint64 version = 1; } }", "Mandate"); err == nil {
		t.Fatal("nested-only names impersonated the required top-level message")
	}
}

func TestIntegerContractUnsupportedProtoWireShapesAndCollisionsRefuse(t *testing.T) {
	for _, field := range []string{
		"map<string, uint64> future_counts = 7;",
		"oneof future { uint64 future_counter = 7; }",
		"repeated uint64 future_counts = 7;",
		`uint64 future_counter = 7 [json_name = "futureCounter"];`,
		"uint64 version = 7;",
	} {
		if _, err := integerProtoFields("message Mandate { uint64 version = 4; "+field+" }", "Mandate"); err == nil {
			t.Fatal("unsupported or colliding direct proto shape silently removed coverage", field)
		}
	}
	if _, err := integerProtoFields("message Mandate { uint64 version = 4; } message Mandate { uint64 revision = 1; }", "Mandate"); err == nil {
		t.Fatal("duplicate top-level message silently selected one origin")
	}
	if _, err := integerTSFields("interface MandateDocument { version: string; version: number }", "MandateDocument"); err == nil {
		t.Fatal("duplicate client property silently selected one representation")
	}
}

func TestIntegerContractBoundedAgeRequiresItsNamedProducerAndEveryInitializer(t *testing.T) {
	baseline := integerWireSources(t)
	if !integerBoundedAge(baseline) {
		t.Fatal("current bounded age producer was not resolved")
	}
	age := "kanz/services/accounting/internal/custody/custody.go"
	view := "kanz/services/accounting/internal/server/custody.go"
	for _, mutation := range []struct{ name, path, before, after string }{
		{"unsafe earlier return", view, "func toBreakView(b custody.Break, now time.Time) breakView {", "func toBreakView(b custody.Break, now time.Time) breakView {\n if now.UnixNano() > 0 { return breakView{AgeSeconds: now.UnixNano()} }"},
		{"different named receiver", age, "func (b Break) Age(", "type unrelatedAgeBreak Break\n\nfunc (b unrelatedAgeBreak) Age("},
		{"foreign duration package", age, `"time"`, `time "example.com/foreign/time"`},
		{"foreign view time", view, `"time"`, `time "example.com/foreign/time"`},
		{"foreign Break package", view, `"github.com/eighred/kanz/services/accounting/internal/custody"`, `custody "example.com/foreign/custody"`},
		{"aliased Break declaration", age, "type Break struct {", "type Break = sourceBreak\n\ntype sourceBreak struct {"},
		{"duration result alias", age, "func (b Break) Age(now time.Time) time.Duration {", "type durationAlias = time.Duration\n\nfunc (b Break) Age(now time.Time) durationAlias {"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			sources := maps.Clone(baseline)
			if !strings.Contains(sources[mutation.path], mutation.before) {
				t.Fatal("age premise mutation lost its named production edge")
			}
			sources[mutation.path] = strings.Replace(sources[mutation.path], mutation.before, mutation.after, 1)
			if integerBoundedAge(sources) {
				t.Fatal("changed age premise retained the numeric wire exception")
			}
			if problems := strings.Join(integerWireProblems(sources), "\n"); !strings.Contains(problems, "CustodyBreak.age_seconds") {
				t.Fatal("changed age premise did not reach the client contract", problems)
			}
		})
	}
}
