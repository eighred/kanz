package replay

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ptrInt64(v int64) *int64        { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

func TestRangeValidate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		r       Range
		wantErr string
	}{
		{"end-bound required", Range{}, "requires an end bound"},
		{"start offset+time exclusive", Range{StartOffset: ptrInt64(0), StartTime: ptrTime(now), EndOffset: ptrInt64(10)}, "StartOffset and Range.StartTime"},
		{"end offset+time exclusive", Range{EndOffset: ptrInt64(10), EndTime: ptrTime(now)}, "EndOffset and Range.EndTime"},
		{"negative start offset", Range{StartOffset: ptrInt64(-1), EndOffset: ptrInt64(10)}, "StartOffset must be >= 0"},
		{"negative end offset", Range{EndOffset: ptrInt64(-1)}, "EndOffset must be >= 0"},
		{"inverted offsets", Range{StartOffset: ptrInt64(5), EndOffset: ptrInt64(3)}, "EndOffset must be >= StartOffset"},
		{"inverted times", Range{StartTime: ptrTime(now), EndTime: ptrTime(now.Add(-time.Second))}, "EndTime must be after StartTime"},
		{"equal times", Range{StartTime: ptrTime(now), EndTime: ptrTime(now)}, "EndTime must be after StartTime"},
		{"offset-only ok", Range{StartOffset: ptrInt64(0), EndOffset: ptrInt64(10)}, ""},
		{"time-only ok", Range{StartTime: ptrTime(now), EndTime: ptrTime(now.Add(time.Minute))}, ""},
		{"default-start offset-end ok", Range{EndOffset: ptrInt64(10)}, ""},
		{"default-start time-end ok", Range{EndTime: ptrTime(now)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewReaderValidation(t *testing.T) {
	end := int64(10)
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"no brokers", Config{Topic: "t", Range: Range{EndOffset: &end}}, "at least one broker"},
		{"no topic", Config{Brokers: []string{"b:1"}, Range: Range{EndOffset: &end}}, "topic required"},
		{"bad range", Config{Brokers: []string{"b:1"}, Topic: "t"}, "requires an end bound"},
		{"negative partition", Config{Brokers: []string{"b:1"}, Topic: "t", Range: Range{EndOffset: &end}, Partitions: []int{-1}}, "partition -1 invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewReaderDefaults(t *testing.T) {
	end := int64(10)
	r, err := NewReader(Config{
		Brokers: []string{"b:1"},
		Topic:   "t",
		Range:   Range{EndOffset: &end},
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.cfg.PerPartitionBuffer != 64 {
		t.Errorf("PerPartitionBuffer=%d want 64", r.cfg.PerPartitionBuffer)
	}
	if r.cfg.PartitionDialTimeout != 10*time.Second {
		t.Errorf("PartitionDialTimeout=%v want 10s", r.cfg.PartitionDialTimeout)
	}
}

func TestResolvePartitionsExplicitDedupSort(t *testing.T) {
	end := int64(10)
	r, err := NewReader(Config{
		Brokers:    []string{"b:1"},
		Topic:      "t",
		Range:      Range{EndOffset: &end},
		Partitions: []int{3, 1, 2, 1, 3, 0},
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// context.Background(), not nil: this path does not dial today, but a nil ctx is a
	// panic waiting for the day it does.
	got, err := r.resolvePartitions(context.Background())
	if err != nil {
		t.Fatalf("resolvePartitions: %v", err)
	}
	want := []int{0, 1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
