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
