package natsrebuild

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/tools/replay"
)

// WindowFor returns the read range for one topic.
//
// EVENT topics (Kafka cleanup.policy=delete) are rebuilt from a bounded recent
// window: the live tier is short-retention by design and a DR spine needs the
// recent history, not all of it.
//
// STATE topics (cleanup.policy=compact, retention -1) must be read IN FULL.
// Compaction retains the latest record per key regardless of age, so a time
// lower bound skips any key not written inside the window — a mandate armed
// last week would not come back, and a compliance control that returns
// DISARMED is the failure infra/kafka/topics-job.yaml warns about in writing.
//
// The end bound stays wall-clock for both: it means "everything up to now",
// and replay.Reader already stops at min(bound, high-water-mark) so a range
// past the log's end cannot block (DATA-M6).
func WindowFor(topic string, state map[string]bool, since time.Duration, now time.Time) replay.Range {
	end := now
	if state[topic] {
		var earliest int64 // 0 — the earliest retained offset, not "no bound"
		return replay.Range{StartOffset: &earliest, EndTime: &end}
	}
	start := now.Add(-since)
	return replay.Range{StartTime: &start, EndTime: &end}
}

// RequireStateTopics refuses an empty state-topic list. Compacted topics
// (compliance.mandate, risk.position) retain only the latest record per key
// regardless of age; reading them through NATS_REBUILD_SINCE's bounded window
// instead of in full loses every key not rewritten inside that window — a
// mandate armed last week does not come back, and the compliance control it
// backs returns DISARMED. An empty NATS_REBUILD_STATE_TOPICS has no orphans,
// so ValidateTopicClasses' subset check alone would pass it; this is the
// separate, symmetric-in-spirit check that fails closed on the empty case
// instead. The run still exits 0 and reports a plausible non-zero published
// count when this is skipped, which is what makes an empty list dangerous
// rather than merely useless: refuse to start rather than produce a restore
// that looks complete but silently dropped every compacted key.
func RequireStateTopics(stateTopics []string) error {
	if len(stateTopics) == 0 {
		return fmt.Errorf(
			"NATS_REBUILD_STATE_TOPICS is required and must not be empty: compacted topics retain " +
				"only the latest record per key regardless of age, so reading them through " +
				"NATS_REBUILD_SINCE's time window instead of in full would silently lose any key not " +
				"rewritten inside that window — e.g. a mandate armed before the window, which would " +
				"not come back, leaving the compliance control it backs DISARMED while the run still " +
				"exits 0")
	}
	return nil
}

// ValidateTopicClasses refuses a configuration whose state-topic list is not a
// subset of the topics being rebuilt. Failing closed matters here: a state
// topic nobody reads restores nothing, and the run still exits 0, so the
// operator is told the spine is rebuilt when its controls are missing.
func ValidateTopicClasses(topics, stateTopics []string) error {
	var orphans []string
	for _, s := range stateTopics {
		if !slices.Contains(topics, s) {
			orphans = append(orphans, s)
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf(
			"natsrebuild: NATS_REBUILD_STATE_TOPICS names %s, which NATS_REBUILD_TOPICS does not rebuild — "+
				"a state topic that is not read restores nothing while the run still succeeds",
			strings.Join(orphans, ", "))
	}
	return nil
}
