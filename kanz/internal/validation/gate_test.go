package validation

import (
	"errors"
	"testing"
	"time"
)

var v0 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// A worked-example style case set: the Hull European-call benchmark shape
// (S=100, K=100, r=5%, σ=20%, T=1 ⇒ 10.4506) — the analytic's output arrives
// as data, so the gate stays import-free of the pricers it validates.
func hullCases(got float64) []Case {
	return []Case{{Name: "hull_bs_call_atm", Got: got, Want: 10.4506, Tolerance: 5e-4}}
}

func TestValidate_PassAndFail(t *testing.T) {
	pass, err := Validate("pricing.BlackScholes", hullCases(10.45058), v0, 0, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !pass.Passed || pass.Signature == "" {
		t.Fatalf("reproducing the benchmark must pass and be signed: %+v", pass)
	}
	if want := v0.Add(DefaultValidity); !pass.ExpiresAt.Equal(want) {
		t.Errorf("default validity: got %v want %v", pass.ExpiresAt, want)
	}

	fail, err := Validate("pricing.BlackScholes", hullCases(10.9), v0, 0, nil)
	if err != nil {
		t.Fatalf("Validate (failing analytic): %v", err)
	}
	if fail.Passed {
		t.Fatal("missing the benchmark must fail")
	}
	if fail.Signature == "" {
		t.Fatal("a failing report is still signed audit evidence")
	}
	if fail.Signature == pass.Signature {
		t.Fatal("different evidence must sign differently")
	}
}

func TestValidate_Errors(t *testing.T) {
	if _, err := Validate("", hullCases(10.45), v0, 0, nil); !errors.Is(err, ErrValidation) {
		t.Error("unnamed analytic must error")
	}
	if _, err := Validate("x", nil, v0, 0, nil); !errors.Is(err, ErrValidation) {
		t.Error("evidence-free sign-off must error")
	}
	bad := []Case{{Name: "c", Got: 1, Want: 1, Tolerance: 0}}
	if _, err := Validate("x", bad, v0, 0, nil); !errors.Is(err, ErrValidation) {
		t.Error("non-positive tolerance must error")
	}
}

func TestGate_DenyByDefault(t *testing.T) {
	now := v0
	g := NewGate(func() time.Time { return now })

	// Missing ⇒ denied.
	if err := g.Promote("pricing.BlackScholes"); !errors.Is(err, ErrDenied) {
		t.Fatalf("unvalidated analytic must be denied, got %v", err)
	}

	// Failed ⇒ denied (a recorded failure is evidence, not a pass).
	fail, _ := Validate("pricing.BlackScholes", hullCases(10.9), v0, 0, nil)
	if err := g.Record(fail); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := g.Promote("pricing.BlackScholes"); !errors.Is(err, ErrDenied) {
		t.Fatal("failed validation must deny promotion")
	}

	// Passing ⇒ promoted; revalidation supersedes the failure.
	pass, _ := Validate("pricing.BlackScholes", hullCases(10.45058), v0, 0, nil)
	if err := g.Record(pass); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := g.Promote("pricing.BlackScholes"); err != nil {
		t.Fatalf("current passing validation must promote: %v", err)
	}

	// Expired ⇒ denied again (validation is current, not forever).
	now = pass.ExpiresAt
	if err := g.Promote("pricing.BlackScholes"); !errors.Is(err, ErrDenied) {
		t.Fatal("expired validation must deny promotion")
	}
}

func TestGate_RecordRejectsUnsigned(t *testing.T) {
	g := NewGate(nil)
	if err := g.Record(Report{Analytic: "x"}); !errors.Is(err, ErrValidation) {
		t.Error("unsigned report must be rejected")
	}
	if err := g.Record(Report{Signature: "abc"}); !errors.Is(err, ErrValidation) {
		t.Error("unnamed report must be rejected")
	}
}
