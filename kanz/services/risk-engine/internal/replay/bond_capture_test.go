package replay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/compute"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func TestBondCanonicalCaptureDoesNotAliasProviderOrReplay(t *testing.T) {
	source := &bondSource{status: compute.TermsResolved, spec: compute.BondSpec{Face: 100, CanonicalTerms: &referencepb.BondTerms{FaceValue: &commonpb.Decimal{Coefficient: 100}}}}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := &inputs{values: make(map[string]json.RawMessage)}
	ctx := withInputs(context.Background(), in)
	provider := Bonds{Source: source}
	first, status := provider.BondTerms(ctx, "bond", at)
	if status != compute.TermsResolved || first.CanonicalTerms == nil {
		t.Fatal("capture lost original canonical terms")
	}
	first.CanonicalTerms.FaceValue.Coefficient = 1
	if source.spec.CanonicalTerms.FaceValue.Coefficient != 100 {
		t.Fatal("consumer mutated live terms provider")
	}
	second, status := provider.BondTerms(ctx, "bond", at)
	if status != compute.TermsResolved || second.CanonicalTerms.FaceValue.Coefficient != 100 || source.calls != 1 {
		t.Fatal("memoized replay terms changed or were re-resolved")
	}
	// Older retained records did not carry canonical terms. Keep that absence
	// explicit; analytical floats cannot recover the original Decimal encoding.
	var old compute.BondSpec
	if err := json.Unmarshal([]byte(`{"Face":100,"CouponRate":0}`), &old); err != nil || old.CanonicalTerms != nil {
		t.Fatal("legacy replay invented canonical terms")
	}
}
