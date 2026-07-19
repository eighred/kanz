package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A tenant's topic table is the system table, prefixed. It is not a second table.
//
// infra/kafka/topics-job.yaml carries a written record of its own correction: the
// table it used to hold named execution.order, market.equity, market.option and
// data.market_stream, "NOT ONE of which is published by any service", while the
// entire money path — orders, fills, accounting, settlement, mandates, positions
// — had no topic at all. infra/kafka/tenancy.yaml was still carrying that exact
// discredited table, so every tenant after the first would have been provisioned
// 6 topics nobody publishes and none of the money path.
//
// It was latent only because the estate's one tenant is __system__, which is
// UN-prefixed and provisioned by topics-job.yaml instead — so the tenant path had
// never run. It fires on the first real tenant, and it fails CLOSED: the archiver
// maps every event to {tenant}.{domain}.{entity} (services/archiver/internal/
// topic/topic.go:64) and Kafka auto-create is disabled, so a missing topic is a
// hard publish failure and a NACK loop, not a silent drop.
//
// Neither table is derivable from the other by machine — one is applied per
// cluster and one per tenant, by different Jobs — so the only thing that keeps
// them honest is this comparison.

// tenantExemptTopics are system topics that deliberately get NO tenant-prefixed
// sibling. A name here needs a reason that survives a reader asking "why would a
// tenant not need this one?".
var tenantExemptTopics = map[string]string{
	"dlq.archiver": "`dlq` is a RESERVED leading segment — topic.For refuses to archive dlq.* at all " +
		"(topic.go:28,50-53), so no {tenant}.dlq.archiver can ever be produced. Per-tenant DLQs are " +
		"created by the `dlq` column instead, as {tenant}.dlq.{name}.",
}

// topicSpec is one row of a topic table.
type topicSpec struct {
	partitions string
	cleanup    string
	retention  string
	dlq        string
}

func (s topicSpec) String() string {
	return fmt.Sprintf("partitions=%s cleanup=%s retention=%s dlq=%s", s.partitions, s.cleanup, s.retention, s.dlq)
}

// topicTableRow captures every column, not just the name: a tenant whose orders
// compact away, or whose ledger retains for a different window than the system
// cluster, is a divergence worth failing the build over.
var topicTableRow = regexp.MustCompile(
	`(?m)^\s{4}([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+(\d+)\s+(delete|compact)\s+(-?\d+)\s+(yes|no)\s*$`)

func parseTopicTable(t *testing.T, path string) map[string]topicSpec {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]topicSpec{}
	for _, m := range topicTableRow.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = topicSpec{partitions: m[2], cleanup: m[3], retention: m[4], dlq: m[5]}
	}
	return out
}

func TestTenantTopicTableMatchesTheSystemTable(t *testing.T) {
	root := moduleRoot(t)
	system := parseTopicTable(t, filepath.Join(root, "infra", "kafka", "topics-job.yaml"))
	tenant := parseTopicTable(t, filepath.Join(root, "infra", "kafka", "tenancy.yaml"))

	// NON-VACUITY: a parser that matched nothing would make the two tables look
	// identical (both empty) and pass this guard no matter how far they drifted.
	if len(system) == 0 {
		t.Fatal("no rows parsed from topics-job.yaml — the table format changed and this guard is now blind")
	}
	if len(tenant) == 0 {
		t.Fatal("no rows parsed from tenancy.yaml — the table format changed and this guard is now blind")
	}
	// And a known money-path topic must be readable, or the parse is subtly wrong.
	if _, ok := system["order.order"]; !ok {
		t.Fatalf("order.order not parsed from topics-job.yaml — parser regression; got %d rows", len(system))
	}

	var problems []string

	for name, want := range system {
		if reason, exempt := tenantExemptTopics[name]; exempt {
			if _, present := tenant[name]; present {
				problems = append(problems, fmt.Sprintf(
					"%s: present in tenancy.yaml but declared tenant-exempt — remove it from one or the other.\n      reason on file: %s",
					name, reason))
			}
			continue
		}
		got, present := tenant[name]
		if !present {
			problems = append(problems, fmt.Sprintf(
				"%s: provisioned per-cluster but NOT per-tenant — a tenant publishing it hits a topic that does not "+
					"exist, and auto-create is disabled", name))
			continue
		}
		if got != want {
			problems = append(problems, fmt.Sprintf(
				"%s: settings differ.\n      topics-job.yaml: %s\n      tenancy.yaml:    %s", name, want, got))
		}
	}

	for name := range tenant {
		if _, present := system[name]; present {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"%s: provisioned per-tenant but absent from topics-job.yaml — either nothing publishes it (the exact "+
				"defect topics-job.yaml documents correcting) or the system table is missing it", name))
	}

	for name := range tenantExemptTopics {
		if _, present := system[name]; !present {
			problems = append(problems, fmt.Sprintf(
				"%s: declared tenant-exempt but not in topics-job.yaml at all — dead exemption, remove it", name))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("the per-tenant and per-cluster topic tables have drifted (%d):\n\n    %s\n\n"+
			"infra/kafka/tenancy.yaml must provision the same topics as infra/kafka/topics-job.yaml, with the "+
			"same settings, under the tenant prefix — the archiver routes EVERY event to "+
			"{tenant}.{domain}.{entity} and Kafka auto-create is disabled, so anything missing here is a hard "+
			"publish failure for that tenant. Add the topic to tenancy.yaml, or declare it in "+
			"tenantExemptTopics with a reason.", len(problems), strings.Join(problems, "\n    "))
	}
}
