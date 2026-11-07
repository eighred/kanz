// Package natsconfig renders offline candidates from the reviewed, embedded
// broker account baseline. It never loads a live broker configuration.
package natsconfig

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/nats-io/nats-server/v2/conf"
	"gopkg.in/yaml.v3"
)

//go:embed tenancy.yaml
var tenancyYAML []byte

const baselineSHA256 = "bb9b93b765642f178ed8d90304f47da364ed37209a61bc7fe95dd4e66254d48a"
const SnapshotIdentity = "spiffe://kanz.internal/ns/kanz-services/sa/lake-sink"
const SnapshotStream = snapshotprofile.Name
const SnapshotDurable = snapshotprofile.Durable

var accountPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var errCandidate = errors.New("snapshot account candidate refused")

// SnapshotBundle is an offline bounded artifact, not Kubernetes apply input.
// The operator/proof separately provisions the exact stream in Account using
// an administrative principal; the snapshot workload has no such permissions.
func SnapshotBundle(account string) ([]byte, error) {
	candidate, err := SnapshotCandidate(account)
	if err != nil {
		return nil, err
	}
	var configMap map[string]any
	if yaml.Unmarshal(candidate, &configMap) != nil {
		return nil, errCandidate
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(map[string]any{"schema_version": 1, "account": account, "config_map": configMap, "stream": snapshotprofile.Stream(), "archiver_environment": map[string]string{"ARCHIVER_LAKE_SNAPSHOTS": "true"}}) != nil || output.Len() > 512<<10 {
		return nil, errCandidate
	}
	return output.Bytes(), nil
}

// SnapshotCandidate adds exactly one explicit mapping to an EXISTING tenant
// account. Its output is an offline tenants.conf candidate, never applied or
// reloaded here. The default compute set and Kafka identities are untouched.
func SnapshotCandidate(account string) ([]byte, error) {
	config, err := snapshotAccounts(account)
	if err != nil {
		return nil, err
	}
	// Preserve every ConfigMap field; only its embedded tenants.conf changes.
	var document yaml.Node
	if yaml.Unmarshal(tenancyYAML, &document) != nil {
		return nil, errCandidate
	}
	root := document.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "data" {
			continue
		}
		data := root.Content[i+1]
		for j := 0; j < len(data.Content); j += 2 {
			if data.Content[j].Value == "tenants.conf" {
				data.Content[j+1].Value = string(config)
				return yaml.Marshal(&document)
			}
		}
	}
	return nil, errCandidate
}

func snapshotAccounts(account string) ([]byte, error) {
	if !accountPattern.MatchString(account) || account == "sys" {
		return nil, errCandidate
	}
	baseline, err := embeddedBaseline()
	if err != nil {
		return nil, err
	}
	document, err := parseBaseline(baseline)
	if err != nil {
		return nil, err
	}
	accounts, ok := document["accounts"].(map[string]any)
	if !ok || len(document) != 2 || document["system_account"] != "SYS" {
		return nil, errCandidate
	}
	if err := uniqueUsers(accounts); err != nil {
		return nil, err
	}
	target, ok := accounts[account].(map[string]any)
	if !ok {
		return nil, errCandidate
	}
	if _, ok := target["jetstream"].(map[string]any); !ok {
		return nil, errCandidate
	}
	users, ok := target["users"].([]any)
	if !ok {
		return nil, errCandidate
	}
	// No moving or remapping an existing principal, including one already in
	// this account: generation is always from the unchanged reviewed baseline.
	for _, raw := range accounts {
		a := raw.(map[string]any) // uniqueUsers validated every account and user.
		for _, user := range a["users"].([]any) {
			if user.(map[string]any)["user"] == SnapshotIdentity {
				return nil, errCandidate
			}
		}
	}
	// Extend only the already registered same-account tenant archiver. Its
	// existing native JetStream and Kafka DLQ authority remains unchanged.
	archiver := "spiffe://kanz.internal/ns/kanz-services/sa/archiver-" + account
	found := false
	for _, raw := range users {
		user := raw.(map[string]any)
		if user["user"] != archiver {
			continue
		}
		permissions, ok := user["permissions"].(map[string]any)
		if !ok {
			return nil, errCandidate
		}
		subscribe, ok := permissions["subscribe"].(map[string]any)
		if !ok {
			return nil, errCandidate
		}
		allow, ok := subscribe["allow"].([]any)
		if !ok {
			return nil, errCandidate
		}
		for _, value := range allow {
			if value == "lake.snapshot.>" {
				return nil, errCandidate
			}
		}
		subscribe["allow"] = append(allow, "lake.snapshot.>")
		found = true
	}
	if !found {
		return nil, errCandidate
	}
	target["users"] = append(users, snapshotUser())
	if err := uniqueUsers(accounts); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(document)
	encoded := output.Bytes()
	if err != nil || len(encoded) > 512<<10 {
		return nil, errCandidate
	}
	// NATS' own parser must read the serialized candidate as exactly the same
	// data. JSON preserves array order; sorting object keys has no NATS meaning.
	roundtrip, err := parseNative(encoded)
	if err != nil {
		return nil, errCandidate
	}
	if !reflect.DeepEqual(document, roundtrip) {
		return nil, errCandidate
	}
	return append(encoded, '\n'), nil
}

func embeddedBaseline() ([]byte, error) {
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if len(tenancyYAML) > 512<<10 || yaml.Unmarshal(tenancyYAML, &cm) != nil || len(cm.Data) != 1 {
		return nil, errCandidate
	}
	return []byte(cm.Data["tenants.conf"]), nil
}

func parseBaseline(raw []byte) (map[string]any, error) {
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != baselineSHA256 {
		return nil, errCandidate
	}
	// Native ParseWithChecks is NOT side-effect-free: arbitrary input can read
	// includes/environment and overwrite duplicate keys. Only this exact reviewed
	// baseline is admitted, before invoking it. A baseline edit requires a new
	// hash plus review/guard of those parser capabilities, not an automatic hash.
	return parseNative(raw)
}

func parseNative(raw []byte) (map[string]any, error) {
	parsed, err := conf.ParseWithChecks(string(raw))
	if err != nil {
		return nil, errCandidate
	}
	// Tokens marshal their underlying native values without source locations.
	b, err := json.Marshal(parsed)
	if err != nil {
		return nil, errCandidate
	}
	var result map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&result) != nil {
		return nil, errCandidate
	}
	return result, nil
}

func uniqueUsers(accounts map[string]any) error {
	seen := map[string]bool{}
	for name, raw := range accounts {
		if name == "" {
			return errCandidate
		}
		a, ok := raw.(map[string]any)
		if !ok {
			return errCandidate
		}
		users, ok := a["users"].([]any)
		if !ok || len(users) == 0 {
			return errCandidate
		}
		for _, rawUser := range users {
			u, ok := rawUser.(map[string]any)
			if !ok {
				return errCandidate
			}
			id, ok := u["user"].(string)
			if !ok || id == "" || seen[id] {
				return errCandidate
			}
			seen[id] = true
		}
	}
	return nil
}

func snapshotUser() map[string]any {
	return map[string]any{"user": SnapshotIdentity, "permissions": map[string]any{
		"publish": map[string]any{"allow": []any{
			"lake.snapshot.requested", "lake.snapshot.capturing", "lake.snapshot.published", "lake.snapshot.failed",
			"dlq.lake.snapshot.requested", "$JS.API.STREAM.INFO.DLQ",
			"$SYS.REQ.USER.INFO", "$JS.API.STREAM.NAMES", "$JS.API.STREAM.INFO." + SnapshotStream,
			"$JS.API.CONSUMER.INFO." + SnapshotStream + ".archiver-lake_snapshot__",
			"$JS.API.CONSUMER.INFO." + SnapshotStream + "." + SnapshotDurable,
			"$JS.API.CONSUMER.CREATE." + SnapshotStream + "." + SnapshotDurable,
			"$JS.API.CONSUMER.CREATE." + SnapshotStream + "." + SnapshotDurable + ".lake.snapshot.requested",
			"$JS.API.CONSUMER.MSG.NEXT." + SnapshotStream + "." + SnapshotDurable,
			"$JS.ACK." + SnapshotStream + "." + SnapshotDurable + ".>",
		}},
		"subscribe": map[string]any{"allow": []any{"lake.snapshot.requested", "_INBOX.>"}},
	}}
}
