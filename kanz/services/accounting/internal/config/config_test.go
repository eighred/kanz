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
		"ACCOUNTING_FILL_SUBJECTS", "ACCOUNTING_CASH_SUBJECTS", "ACCOUNTING_DATABASE_URL", "ACCOUNTING_BASE_CURRENCY",
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
	if !reflect.DeepEqual(cfg.CashSubjects, DefaultCashSubjects) {
		t.Errorf("CashSubjects=%v want %v", cfg.CashSubjects, DefaultCashSubjects)
	}
	if cfg.BaseCurrency != "USD" {
		t.Errorf("BaseCurrency=%q want USD", cfg.BaseCurrency)
	}
}

func TestLoadFXDefaults(t *testing.T) {
	// WIRE-01d: FX is off by default (no pairs), FX subjects default to the
	// MARKET FX wildcard, and the instrument-currency map is empty.
	for _, k := range []string{"ACCOUNTING_FX_PAIRS", "ACCOUNTING_FX_SUBJECTS", "ACCOUNTING_INSTRUMENT_CURRENCY"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FXPairs != "" || cfg.InstrumentCurrency != "" {
		t.Errorf("FX pairs/instrument-currency should be empty by default, got %q / %q", cfg.FXPairs, cfg.InstrumentCurrency)
	}
	if !reflect.DeepEqual(cfg.FXSubjects, DefaultFXSubjects) {
		t.Errorf("FXSubjects=%v want %v", cfg.FXSubjects, DefaultFXSubjects)
	}
}

func TestLoadFXOverride(t *testing.T) {
	t.Setenv("ACCOUNTING_FX_PAIRS", "EURUSD:EUR,GBPUSD:GBP")
	t.Setenv("ACCOUNTING_FX_SUBJECTS", "market.fx.quote , market.fx.spot")
	t.Setenv("ACCOUNTING_INSTRUMENT_CURRENCY", "SAP:EUR")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FXPairs != "EURUSD:EUR,GBPUSD:GBP" {
		t.Errorf("FXPairs=%q", cfg.FXPairs)
	}
	want := []string{"market.fx.quote", "market.fx.spot"}
	if !reflect.DeepEqual(cfg.FXSubjects, want) {
		t.Errorf("FXSubjects=%v want %v", cfg.FXSubjects, want)
	}
	if cfg.InstrumentCurrency != "SAP:EUR" {
		t.Errorf("InstrumentCurrency=%q", cfg.InstrumentCurrency)
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
