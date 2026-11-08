package provision

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

const testManifest = "data:\n  topics.sh: |\n    never execute this\n    done <<'EOF'\n    order.order 6 delete 2592000000 yes\n    risk.position 6 compact -1 yes\n    dlq.archiver 3 delete 2592000000 no\n    EOF\n"

func manifestDigest(b string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(b))) }

func TestTableTenantAndSystem(t *testing.T) {
	for _, tenant := range []string{"acme", "__system__"} {
		rows, err := ParseTable([]byte(testManifest), manifestDigest(testManifest), tenant)
		if err != nil {
			t.Fatal(err)
		}
		want, first := 4, "acme.order.order"
		if tenant == "__system__" {
			want, first = 5, "order.order"
		}
		if len(rows) != want || rows[0].Name != first || rows[0].Partitions != 6 || rows[2].Cleanup != "compact" || rows[2].Retention != -1 {
			t.Fatal("canonical prefix or topic settings changed")
		}
	}
}

func TestTrackedCanonicalManifestParsesWithoutThirdTable(t *testing.T) {
	raw, err := os.ReadFile("../../../../infra/kafka/topics-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	system, err := ParseTable(raw, manifestDigest(string(raw)), "__system__")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := ParseTable(raw, manifestDigest(string(raw)), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(system) != len(tenant)+1 || len(system) < 40 {
		t.Fatal("canonical inventory or sole exemption changed")
	}
	want := make(map[string]Topic)
	for _, row := range system {
		if row.Name != "dlq.archiver" {
			row.Name = "acme." + row.Name
			want[row.Name] = row
		}
	}
	for _, row := range tenant {
		if want[row.Name] != row {
			t.Fatal("tenant settings differ")
		}
		delete(want, row.Name)
	}
	if len(want) != 0 {
		t.Fatal("tenant topics missing")
	}
}

func TestTableRefusesMalformedOrUnboundInput(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate":           strings.Replace(testManifest, "    EOF", "    order.order 6 delete 2592000000 yes\n    EOF", 1),
		"duplicate-exemption": strings.Replace(testManifest, "    EOF", "    dlq.archiver 3 delete 2592000000 no\n    EOF", 1),
		"second-table":        testManifest + "    done <<'EOF'\n    EOF\n",
		"unterminated":        strings.Replace(testManifest, "    EOF\n", "", 1),
		"shell-row":           strings.Replace(testManifest, "order.order", "$(touch_file)", 1),
		"partition":           strings.Replace(testManifest, "6 delete", "0 delete", 1),
		"retention":           strings.Replace(testManifest, "2592000000", "-2", 1),
		"extra-column":        strings.Replace(testManifest, "2592000000 yes", "2592000000 yes extra", 1),
		"missing-money-row":   strings.ReplaceAll(testManifest, "order.order", "not.order"),
		"bound":               strings.Repeat(" ", MaxManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTable([]byte(raw), manifestDigest(raw), "acme"); err == nil {
				t.Fatal("invalid table accepted")
			}
		})
	}
	if _, err := ParseTable([]byte(testManifest), strings.Repeat("0", 64), "acme"); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	for _, tenant := range []string{"", ".", "acme.other", "Acme", "-acme", "acme-"} {
		if _, err := ParseTable([]byte(testManifest), manifestDigest(testManifest), tenant); err == nil {
			t.Fatal("invalid tenant accepted")
		}
	}
}
