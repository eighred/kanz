package config

import (
	"reflect"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// A clean environment yields the production-safe defaults, including the
	// fill-folding consumer wiring (WIRE-01b).
	for _, k := range []string{
		"ACCOUNTING_NATS_URL", "ACCOUNTING_SOURCE", "ACCOUNTING_CONSUMER_GROUP",
		"ACCOUNTING_FILL_SUBJECTS", "ACCOUNTING_DATABASE_URL", "ACCOUNTING_BASE_CURRENCY",
	} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATSURL != "" {
		t.Errorf("NATSURL=%q want empty (consumer off by default)", cfg.NATSURL)
	}
	if cfg.Source != "accounting" {
		t.Errorf("Source=%q want accounting", cfg.Source)
	}
	if cfg.ConsumerGroup != "accounting" {
		t.Errorf("ConsumerGroup=%q want accounting", cfg.ConsumerGroup)
	}
	if !reflect.DeepEqual(cfg.FillSubjects, DefaultFillSubjects) {
		t.Errorf("FillSubjects=%v want %v", cfg.FillSubjects, DefaultFillSubjects)
	}
	if cfg.BaseCurrency != "USD" {
		t.Errorf("BaseCurrency=%q want USD", cfg.BaseCurrency)
	}
}

func TestLoadFillSubjectsOverride(t *testing.T) {
	t.Setenv("ACCOUNTING_FILL_SUBJECTS", " order.order.filled , , custom.fill ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"order.order.filled", "custom.fill"}
	if !reflect.DeepEqual(cfg.FillSubjects, want) {
		t.Errorf("FillSubjects=%v want %v (trimmed, empties dropped)", cfg.FillSubjects, want)
	}
}
