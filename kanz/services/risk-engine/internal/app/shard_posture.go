package app

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// IS THIS FLEET SHARDED, AND DOES IT NEED TO BE (#110)?
//
// # What the ring is for
//
// ShardFilter's own doc states it: the consistent-hash ring is "the fan-out that
// pins each portfolio's RISK-05 per-aggregate state to a single replica". That
// pinning is not an optimisation. internal/risk/state.Store is a PER-REPLICA
// IN-MEMORY map of portfolios, and the ingest path folds each state event into
// it — so a portfolio's state is only complete on a replica that saw every one
// of that portfolio's events.
//
// # What happens without it
//
// Unsharded, every replica subscribes under the SHARED durable consumer
// (DefaultConsumerGroup), and its own comment says what that does: "scaling to N
// replicas spreads partitions within the group". The events carry the portfolio
// id as their partition key — but on the NATS spine a queue group distributes to
// whichever puller is free, and the key rides as a transport header rather than
// selecting a consumer.
//
// So with replicas > 1 and no ring, a portfolio's events are split across
// replicas, and each holds a PARTIAL view of it. Whichever replica handles the
// next event recomputes that portfolio's risk from the part of the book it
// happens to have seen.
//
// THAT IS NOT VISIBLE ANYWHERE. The measures are produced, published and stored;
// nothing errors, no probe fails, and the number is wrong only in the sense that
// it was computed from an incomplete position set — which looks exactly like a
// number computed from a complete one. This is the estate's recurring shape:
// "nothing configured" and "checked, and fine" must never look the same.
//
// # What this does NOT do
//
// IT DOES NOT ENABLE SHARDING, and the reason is worth recording because it is
// the actual blocker behind #110 rather than an oversight. A consistent-hash
// ring needs the FULL MEMBER LIST at every replica, and RISK_ENGINE_SHARD_MEMBERS
// is a static environment value. risk-engine is a Rollout scaled by KEDA from 3
// to 12 — its pod names are assigned at schedule time and change on every
// reschedule, so no static list can name them, and a list that is wrong is worse
// than none: a replica missing from the ring owns nothing and silently stops
// applying, while one named twice splits a portfolio it was supposed to pin.
//
// Wiring it needs membership DISCOVERY (a StatefulSet's stable ordinals, or a
// registry the replicas join), which does not exist here. So this states the
// posture instead, and the metric is what makes the gap countable rather than
// something a reader has to derive from two files and a NATS semantic.
//
// # What the estate now refuses rather than absorbs
//
// The gap above is a DECISION that is still open; the failures around it are
// not. shard.NewAssignment refuses every half-configured ring — an id absent
// from the list, a list with no id, an id with no list — and the composition
// root exits on it before any I/O, because each of those silently produced a
// replica that owned nothing (or everything) with green probes. And
// test/arch/shard_membership_is_not_static_test.go forbids pairing a
// hand-written member list with a KEDA-scaled workload, which is what "fixing
// #110" looks like from the outside and is strictly worse than this posture.

// ShardPosture reports whether this replica is sharding, and warns when it is
// not.
//
// selfKnown is whether RISK_ENGINE_SHARD_SELF is set; members is the configured
// ring. Both are reported, because "no members" and "members but no identity"
// are different misconfigurations with the same symptom — an unsharded replica —
// and an operator fixing one needs to know which they have.
// foreignSkipped is how many durable portfolio records this replica declined to
// restore at boot because the ring assigns them elsewhere
// (Bootstrap.ForeignRecordsSkipped). It is reported ALWAYS, including the zero,
// because zero is the interesting reading: on a sharded replica of a fleet with
// a populated database it means the ring is not actually splitting the book, and
// a series that only appears once it is non-zero cannot say that.
func ShardPosture(reg prometheus.Registerer, logger *slog.Logger, sharded bool, members []string, self string, foreignSkipped int) {
	// A COUNT THAT ONLY THIS PATH CAN PRODUCE. Before #110 wired the ring into
	// the store, a sharded replica restored every portfolio in the tenant's
	// database and then wrote its frozen copies back over the owners' records
	// on the next checkpoint. This gauge is the evidence that no longer happens:
	// it is the number of records the boot path handed back instead of holding.
	skipped := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_risk_shard_foreign_records_skipped",
		Help: "Durable portfolio records this replica did NOT restore at boot because the shard " +
			"ring assigns them to another replica. Zero on an unsharded replica (it owns " +
			"everything). Zero on a SHARDED replica whose database holds other replicas' " +
			"portfolios means the ring is not splitting the book (#110).",
	})
	reg.MustRegister(skipped)
	skipped.Set(float64(foreignSkipped))

	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_risk_shard_enabled",
		Help: "1 when this replica filters to a consistent-hash shard, 0 when it owns everything. " +
			"A ZERO ON A MULTI-REPLICA FLEET means each replica holds a partial view of every " +
			"portfolio: the risk store is per-replica and in-memory, and the shared consumer group " +
			"splits a portfolio's events across replicas rather than pinning them (#110).",
	}, []string{"self"})
	reg.MustRegister(g)

	// The member count is a SECOND series rather than a label on the first,
	// because it answers a different question: not "is this replica sharding"
	// but "how many does it think there are". A ring of one is sharding and is
	// still a single-instance fleet.
	c := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_risk_shard_members",
		Help: "Number of members in this replica's configured shard ring. Zero means unsharded. " +
			"A count that disagrees with the actual replica count is the dangerous state: a replica " +
			"missing from the ring owns nothing and silently stops applying (#110).",
	})
	reg.MustRegister(c)
	c.Set(float64(len(members)))

	label := self
	if label == "" {
		label = "unset"
	}
	if sharded {
		g.WithLabelValues(label).Set(1)
		logger.Info("risk-engine sharding enabled", "self", self, "members", members)
		return
	}
	g.WithLabelValues(label).Set(0)

	// WARN, not Info. "Every replica is computing risk from a partial book" is a
	// sentence that has to have been read before anyone trusts an exposure
	// number, and Info is where it gets filtered out.
	//
	// The message names the MECHANISM rather than the setting, because an
	// operator who reads "sharding is off" reasonably concludes it is a
	// performance choice.
	logger.Warn("risk-engine is NOT sharded — every replica owns every portfolio, and the risk store "+
		"is per-replica and in-memory. With more than one replica the shared consumer group splits "+
		"a portfolio's events between them, so each holds a PARTIAL view and recomputes from the "+
		"part it happened to receive. Nothing errors and no probe fails",
		"members", len(members), "self", self,
		"why_not_wired", "a consistent-hash ring needs the full member list at every replica, and a "+
			"KEDA-scaled Rollout's pod names are not knowable statically — it needs membership "+
			"discovery (#110)",
		"gauge", "kanz_risk_shard_enabled=0")
}
