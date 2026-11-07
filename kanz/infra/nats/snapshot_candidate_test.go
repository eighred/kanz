package natsconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/nats-io/nats.go/jetstream"
	"gopkg.in/yaml.v3"
)

func TestSnapshotBundleBindsExistingAccountAndExactRuntimeProfile(t *testing.T) {
	wire, err := SnapshotBundle("acme")
	if err != nil || len(wire) > 512<<10 {
		t.Fatal("bounded candidate bundle unavailable")
	}
	var bundle struct {
		SchemaVersion       int                    `json:"schema_version"`
		Account             string                 `json:"account"`
		ConfigMap           map[string]any         `json:"config_map"`
		Stream              jetstream.StreamConfig `json:"stream"`
		ArchiverEnvironment map[string]string      `json:"archiver_environment"`
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if d.Decode(&bundle) != nil || bundle.SchemaVersion != 1 || bundle.Account != "acme" || !reflect.DeepEqual(bundle.Stream, snapshotprofile.Stream()) || !reflect.DeepEqual(bundle.ArchiverEnvironment, map[string]string{"ARCHIVER_LAKE_SNAPSHOTS": "true"}) {
		t.Fatal("bundle identity/profile differs from runtime requirement")
	}
	candidate, err := SnapshotCandidate("acme")
	var expected map[string]any
	if err != nil || yaml.Unmarshal(candidate, &expected) != nil || !reflect.DeepEqual(expected, bundle.ConfigMap) {
		t.Fatal("bundle changed the independently verified ConfigMap")
	}
	// Every construction owns its subject slice; a renderer cannot mutate the
	// next runtime's authoritative requirement through shared backing storage.
	changed := snapshotprofile.Stream()
	changed.Subjects[0] = "foreign.>"
	if snapshotprofile.Stream().Subjects[0] != "lake.snapshot.requested" {
		t.Fatal("shared mutable profile")
	}
}

func TestSnapshotCandidatePreservesBaselineAndAddsOneAccountPrincipal(t *testing.T) {
	output, err := SnapshotCandidate("acme")
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if yaml.Unmarshal(tenancyYAML, &before) != nil || yaml.Unmarshal(output, &after) != nil {
		t.Fatal("invalid ConfigMap")
	}
	original := before["data"].(map[string]any)["tenants.conf"].(string)
	candidate := after["data"].(map[string]any)["tenants.conf"].(string)
	after["data"] = before["data"]
	if !reflect.DeepEqual(before, after) {
		t.Fatal("ConfigMap metadata changed")
	}
	old, err := parseBaseline([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := parseNative([]byte(candidate))
	if err != nil {
		t.Fatal(err)
	}
	accounts := updated["accounts"].(map[string]any)
	account := accounts["acme"].(map[string]any)
	users := account["users"].([]any)
	if !reflect.DeepEqual(users[len(users)-1], snapshotUser()) {
		t.Fatal("mapping/permissions differ")
	}
	account["users"] = users[:len(users)-1]
	archiverFound := false
	for _, raw := range account["users"].([]any) {
		user := raw.(map[string]any)
		if user["user"] != "spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme" {
			continue
		}
		subscribe := user["permissions"].(map[string]any)["subscribe"].(map[string]any)
		allow := subscribe["allow"].([]any)
		if allow[len(allow)-1] != "lake.snapshot.>" {
			t.Fatal("missing exact existing-archiver route")
		}
		subscribe["allow"] = allow[:len(allow)-1]
		archiverFound = true
	}
	if !archiverFound {
		t.Fatal("existing tenant archiver missing")
	}
	if !reflect.DeepEqual(old, updated) {
		t.Fatal("unrelated account semantics/order changed")
	}
	if bytes.Contains(output, []byte("password:")) {
		t.Fatal("candidate introduced credential")
	}
}

func TestSnapshotCandidateRefusesUnknownAccountAndChangedBaseline(t *testing.T) {
	for _, account := range []string{"", "SYS", "__system__", "missing", "acme.other", "acme\n"} {
		if _, err := SnapshotCandidate(account); err == nil {
			t.Fatal("unapproved target accepted")
		}
	}
	raw, err := embeddedBaseline()
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"\ninclude \"missing.conf\"", "\nvalue: $KANZ_FIXTURE_ENV", "\nsystem_account: BAD", "\n# even comment changes require baseline review"} {
		changed := append(bytes.Clone(raw), extra...)
		if _, err := parseBaseline(changed); err == nil {
			t.Fatal("changed baseline reached parser")
		}
	}
	doc, err := parseBaseline(raw)
	if err != nil {
		t.Fatal(err)
	}
	accounts := doc["accounts"].(map[string]any)
	a := accounts["acme"].(map[string]any)
	a["users"] = append(a["users"].([]any), accounts["SYS"].(map[string]any)["users"].([]any)[0])
	if uniqueUsers(accounts) == nil {
		t.Fatal("cross-account duplicate principal accepted")
	}
}
