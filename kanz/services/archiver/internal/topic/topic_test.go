package topic_test

import (
	"strings"
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/services/archiver/internal/topic"
)

func env(eventType, tenant string, class envelopepb.EventClass) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventType: eventType, TenantId: tenant, EventClass: class}
}

const fact = envelopepb.EventClass_EVENT_CLASS_FACT
const snapshot = envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT

func TestFor_TenantPrefixed(t *testing.T) {
	got, err := topic.For(env("order.order.submitted", "acme", fact), "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "acme.order.order"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// __system__ carries cross-cutting platform events and keeps the UN-PREFIXED
// legacy topic names (subject-taxonomy.md §6).
func TestFor_SystemTenantIsUnprefixed(t *testing.T) {
	got, err := topic.For(env("platform.mode.changed", "__system__", fact), "__system__")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "platform.mode"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// A snapshot needs COMPACTION while its sibling FACTs need time-retention, so it
// goes to a separate compacted topic (subject-taxonomy.md §5).
func TestFor_SnapshotGoesToCompactedSibling(t *testing.T) {
	got, err := topic.For(env("risk.position.changed", "acme", snapshot), "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if want := "acme.risk.position.snapshot"; got != want {
		t.Fatalf("topic = %q, want %q", got, want)
	}
}

// FAIL CLOSED. Each of these must be an error — an error NACKs, and a NACK keeps
// the event safe in NATS. A guess would misfile it forever.
func TestFor_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		e      *envelopepb.Envelope
		tenant string
		want   string // substring the error must name
	}{
		{"cross-tenant leak", env("order.order.submitted", "evil", fact), "acme", "tenant"},
		{"empty tenant on envelope", env("order.order.submitted", "", fact), "acme", "tenant"},
		{"two segments", env("order.submitted", "acme", fact), "acme", "event_type"},
		{"four segments", env("a.b.c.d", "acme", fact), "acme", "event_type"},
		{"empty event_type", env("", "acme", fact), "acme", "event_type"},
		{"reserved replay prefix", env("replay.run1.order", "acme", fact), "acme", "reserved"},
		{"reserved dlq prefix", env("dlq.order.submitted", "acme", fact), "acme", "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := topic.For(tc.e, tc.tenant)
			if err == nil {
				t.Fatalf("expected an error, got topic %q — this would misfile the event", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
