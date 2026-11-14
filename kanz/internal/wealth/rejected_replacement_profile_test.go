package wealth

import (
	"errors"
	"testing"
)

func TestRejectedReplacementRetainsLastKnownProfileUntilValidRepair(t *testing.T) {
	for _, profile := range []struct {
		name  string
		value RiskProfile
	}{{"unknown", 99}, {"unspecified", ProfileUnspecified}, {"different_known_but_invalid_policy", ProfileBalanced}} {
		t.Run(profile.name, func(t *testing.T) {
			r := NewModelRegistry()
			original := growth()
			if err := r.Put("owner", original); err != nil {
				t.Fatal(err)
			}
			r.Arm()
			bad := original.Clone()
			bad.Profile = profile.value
			bad.Targets = nil
			for range 2 {
				if err := r.Put("owner", bad); err == nil {
					t.Fatal("invalid replacement admitted")
				}
				if _, err := r.Model("owner", ProfileGrowth); !errors.Is(err, ErrModelRejected) {
					t.Fatalf("last known profile became absent or usable: %v", err)
				}
			}
			if err := r.Put("owner", original); err != nil {
				t.Fatal(err)
			}
			if len(r.Rejections()) != 0 {
				t.Fatal("matching repair left refusal behind")
			}
			got, err := r.Model("owner", ProfileGrowth)
			if err != nil || got.Targets["BTC-USD"] != "0.6" {
				t.Fatal(got, err)
			}
		})
	}
}
