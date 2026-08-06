package kafkatest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/kafkatest"
)

// These cover the parts that do not need a broker. The readiness wait itself is
// broker-dependent by construction and is proved by the integration tests that
// call it — see the package doc for why it cannot be proved against a fake.

func TestCreateTopicRefusesAnEmptyBrokerList(t *testing.T) {
	err := kafkatest.CreateTopic(context.Background(), nil, "t", 1)
	if err == nil {
		t.Fatal("CreateTopic accepted an empty broker list; a helper that silently does nothing here " +
			"would leave every caller believing its topic exists")
	}
}

func TestCreateTopicRefusesZeroPartitions(t *testing.T) {
	err := kafkatest.CreateTopic(context.Background(), []string{"localhost:9092"}, "t", 0)
	if err == nil {
		t.Fatal("CreateTopic accepted 0 partitions")
	}
	if !strings.Contains(err.Error(), "partition") {
		t.Errorf("the refusal must name the offending argument; got %v", err)
	}
}

// THE TIMEOUT PATH FAILS LOUDLY. A wait that gave up and returned nil would hand
// the caller a topic that is not there and move the failure to the next produce —
// exactly the confusing error this package exists to remove. Driven with a short
// caller deadline, which WaitReady honours because context.WithTimeout takes the
// earlier of the two.
func TestWaitReadyReportsWhatItSawRatherThanTimingOutSilently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// A port nothing is listening on: the broker can never answer, so this can
	// only end at the deadline.
	err := kafkatest.WaitReady(ctx, []string{"127.0.0.1:1"}, "never.exists", 1)
	if err == nil {
		t.Fatal("WaitReady returned nil for a topic it never saw. A caller would then publish into a " +
			"topic that does not exist and get UNKNOWN_TOPIC_OR_PARTITION, which is the error this " +
			"package exists to stop being mysterious")
	}
	// The message has to distinguish the two end states, or it sends whoever reads
	// it to the wrong place.
	if !strings.Contains(err.Error(), "partition(s) visible") {
		t.Errorf("the error must say what it actually observed, not just that it timed out; got %v", err)
	}
	if !strings.Contains(err.Error(), "never.exists") {
		t.Errorf("the error must name the topic; got %v", err)
	}
}
