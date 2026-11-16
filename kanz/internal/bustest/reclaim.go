package bustest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/nats-io/nats.go/jetstream"
)

// ReclaimSubjects registers exact POSITION-subject cleanup before a fixture
// publishes. Register connection closes first and subscription joins afterwards:
// Cleanup runs in reverse order, so joins precede purge and closes follow it.
// This only removes subjects explicitly owned by the fixture, never a wildcard.
func ReclaimSubjects(t testing.TB, js jetstream.JetStream, subjects ...string) {
	t.Helper()
	if err := reclaimSubjectsValid(subjects); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streams := make(map[string]positionPurger)
	for _, subj := range subjects {
		name, err := js.StreamNameBySubject(ctx, subj)
		if err != nil {
			t.Fatalf("reclaim fixture subject %q has no stream: %v", subj, err)
		}
		if name != "POSITION" {
			t.Fatalf("refuse fixture reclaim from noncanonical stream %q", name)
		}
		stream, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("reclaim fixture stream %q: %v", name, err)
		}
		streams[subj] = stream
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := purgePositionSubjects(cleanup, streams); err != nil {
			t.Error(err)
		}
	})
}

type positionPurger interface {
	Purge(context.Context, ...jetstream.StreamPurgeOpt) error
}

func purgePositionSubjects(ctx context.Context, streams map[string]positionPurger) error {
	var result error
	for subj, stream := range streams {
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject(subj)); err != nil {
			result = errors.Join(result, fmt.Errorf("reclaim fixture subject %q: %w", subj, err))
		}
	}
	return result
}

func reclaimSubjectsValid(subjects []string) error {
	if len(subjects) == 0 || len(subjects) > 256 {
		return fmt.Errorf("reclaim requires 1..256 exact fixture subjects")
	}
	for _, subj := range subjects {
		if len(subj) > 1024 || !strings.HasPrefix(subj, "risk.position.") ||
			strings.ContainsAny(subj, "*>") || strings.ContainsFunc(subj, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			return fmt.Errorf("refuse nonliteral POSITION fixture subject %q", subj)
		}
		for _, token := range strings.Split(subj, ".") {
			if token == "" {
				return fmt.Errorf("refuse empty subject token in %q", subj)
			}
		}
	}
	return nil
}
