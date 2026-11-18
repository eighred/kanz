package terms

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func TestChainMembershipFollowsLatestContractAmendment(t *testing.T) {
	st := NewPostgres(termsTimePool(t))
	ctx := t.Context()
	move := t0.Add(time.Hour)
	reclassify := move.Add(time.Hour)
	original := optionRec("contract", "old", 100, t0, 1)
	moved := optionRec("contract", "new", 110, move, 1)
	future := optionRec("contract", "new", 110, reclassify, 1)
	future.Kind = KindFuture
	future.Terms.Terms = &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{UnderlyingId: "new"}}
	if err := st.Put(ctx, []Record{original, moved, future}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at   time.Time
		want Record
	}{{move.Add(-time.Microsecond), original}, {move, moved}, {reclassify, future}} {
		latest, err := st.LatestAsOf(ctx, "contract", tc.at)
		if err != nil || !proto.Equal(latest.Terms, tc.want.Terms) {
			t.Fatalf("latest version at %v: %v", tc.at, err)
		}
		for _, key := range []struct {
			underlying string
			kind       Kind
		}{{"old", KindOption}, {"new", KindOption}, {"new", KindFuture}} {
			chain, err := st.ChainAsOf(ctx, key.underlying, tc.at, key.kind)
			if err != nil {
				t.Fatal(err)
			}
			if key.underlying == latest.UnderlyingID && key.kind == latest.Kind {
				if len(chain) != 1 || !proto.Equal(chain[0].Terms, latest.Terms) {
					t.Fatalf("current chain lost the latest version at %v", tc.at)
				}
			} else if len(chain) != 0 {
				t.Fatalf("superseded membership at %v in %s/%s", tc.at, key.underlying, key.kind)
			}
		}
	}
}
