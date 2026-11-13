package terms

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func TestTermsRecordRefusesSubmicrosecondEffectiveTime(t *testing.T) {
	for _, at := range []time.Time{t0.Add(time.Nanosecond), t0.Add(999 * time.Nanosecond), t0.Add(1001 * time.Nanosecond), time.Unix(-1, 999_999_999), t0.In(time.FixedZone("offset", 3*3600)).Add(time.Nanosecond)} {
		t.Run(at.Format(time.RFC3339Nano), func(t *testing.T) {
			rec := optionRec("OPTION", "UNDERLYING", 100, at, 1)
			if err := validateRecord(rec); err == nil {
				t.Fatalf("sub-microsecond effective time accepted: %v", at)
			}
		})
	}
}

func TestTermsRecordPreservesExactMicrosecondInstants(t *testing.T) {
	for _, at := range []time.Time{t0, t0.Add(time.Microsecond), time.Unix(0, 0), time.Unix(-1, 999_999_000), t0.In(time.FixedZone("offset", -5*3600)).Add(time.Microsecond)} {
		rec := optionRec("OPTION", "UNDERLYING", 100, at, 1)
		before := proto.Clone(rec.Terms)
		if err := validateRecord(rec); err != nil {
			t.Fatalf("exact microsecond instant refused: %v: %v", at, err)
		}
		if !proto.Equal(before, rec.Terms) || !rec.AsOf.Equal(at) {
			t.Fatal("record validation normalized canonical history")
		}
	}
}

func TestPinnedPGXHorizonEncodingFloorsAroundUnixEpoch(t *testing.T) {
	// PostgreSQL binary timestamps count microseconds from 2000-01-01. These
	// literal offsets independently expose the pinned driver's floor direction.
	for _, tc := range []struct {
		at   time.Time
		want int64
	}{
		{time.Unix(0, -1), -946_684_800_000_001},
		{time.Unix(0, 0), -946_684_800_000_000},
		{time.Unix(0, 1), -946_684_800_000_000},
		{time.Unix(0, 999), -946_684_800_000_000},
		{time.Unix(0, 1000), -946_684_799_999_999},
		{time.Unix(0, 1001), -946_684_799_999_999},
	} {
		encoded, err := pgtype.NewMap().Encode(pgtype.TimestamptzOID, pgtype.BinaryFormatCode, tc.at, nil)
		if err != nil || len(encoded) != 8 {
			t.Fatalf("cannot encode horizon: %v", err)
		}
		if got := int64(binary.BigEndian.Uint64(encoded)); got != tc.want {
			t.Fatalf("horizon %v encoded as %d, want independently %d", tc.at, got, tc.want)
		}
	}
}

type termsRecordMismatch struct {
	name string
	mut  func(*Record)
}

func inconsistentStoredTerms() []termsRecordMismatch {
	return append([]termsRecordMismatch{
		{"lost payload nanosecond", func(r *Record) { r.Terms.AsOf = timestamppb.New(r.AsOf.Add(time.Nanosecond)) }},
		{"different effective time", func(r *Record) { r.Terms.AsOf = timestamppb.New(r.AsOf.Add(time.Hour)) }},
		{"missing effective time", func(r *Record) { r.Terms.AsOf = nil }},
		{"invalid effective timestamp", func(r *Record) { r.Terms.AsOf.Nanos = -1 }},
		{"different instrument", func(r *Record) { r.Terms.InstrumentId = "OTHER" }},
		{"different kind", func(r *Record) { r.Kind = KindFuture }},
		{"missing variant", func(r *Record) { r.Terms.Terms = nil }},
	}, indexedGroupingMismatches()...)
}

func indexedGroupingMismatches() []termsRecordMismatch {
	return []termsRecordMismatch{
		{"different option underlying", func(r *Record) { r.Terms.GetOption().UnderlyingId = "OTHER" }},
		{"different future underlying", func(r *Record) {
			r.Kind = KindFuture
			r.Terms.Terms = &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{UnderlyingId: "OTHER"}}
		}},
		{"different structured deal", func(r *Record) {
			r.Kind = KindStructured
			r.Terms.Terms = &referencepb.ContractTerms_Structured{Structured: &referencepb.StructuredTerms{DealId: "OTHER"}}
		}},
	}
}

func TestTermsRecordRefusesIndexedGroupingDisagreement(t *testing.T) {
	for _, tc := range indexedGroupingMismatches() {
		t.Run(tc.name, func(t *testing.T) {
			rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
			tc.mut(&rec)
			if err := validateRecord(rec); err == nil {
				t.Fatal("indexed grouping key differed from its payload before append")
			}
		})
	}
}

type termsRowFixture struct {
	rec  Record
	blob []byte
}

func (row termsRowFixture) Scan(dest ...any) error {
	*dest[0].(*string) = row.rec.InstrumentID
	*dest[1].(*time.Time) = row.rec.AsOf
	*dest[2].(**string) = &row.rec.UnderlyingID
	*dest[3].(*string) = string(row.rec.Kind)
	*dest[4].(*[]byte) = append([]byte(nil), row.blob...)
	return nil
}

func TestTermsScanRefusesLegacyKeyPayloadDisagreement(t *testing.T) {
	for _, tc := range inconsistentStoredTerms() {
		t.Run(tc.name, func(t *testing.T) {
			rec := optionRec("OPTION", "UNDERLYING", 100, t0, 1)
			tc.mut(&rec)
			blob, err := proto.Marshal(rec.Terms)
			if err != nil {
				t.Fatal(err)
			}
			got, err := scanRecord(termsRowFixture{rec, blob})
			if err == nil || errors.Is(err, ErrNoTerms) || got.Terms != nil {
				t.Fatalf("legacy inconsistency returned usable/missing data: %+v error=%v", got, err)
			}
		})
	}
}

func TestTermsScanPreservesDocumentedGroupingConventions(t *testing.T) {
	for _, kind := range []Kind{KindOption, KindFuture, KindStructured, KindSwap, KindBond} {
		t.Run(string(kind), func(t *testing.T) {
			rec := optionRec("INSTRUMENT", "UNDERLYING", 100, t0, 1)
			rec.Kind = kind
			switch kind {
			case KindFuture:
				rec.Terms.Terms = &referencepb.ContractTerms_Future{Future: &referencepb.FutureTerms{UnderlyingId: "UNDERLYING"}}
			case KindStructured:
				rec.UnderlyingID = "DEAL"
				rec.Terms.Terms = &referencepb.ContractTerms_Structured{Structured: &referencepb.StructuredTerms{DealId: "DEAL"}}
			case KindSwap:
				rec.UnderlyingID = ""
				rec.Terms.Terms = &referencepb.ContractTerms_Swap{Swap: &referencepb.SwapTerms{}}
			case KindBond:
				rec.UnderlyingID = ""
				rec.Terms.Terms = &referencepb.ContractTerms_Bond{Bond: &referencepb.BondTerms{}}
			}
			blob, err := proto.Marshal(rec.Terms)
			if err != nil {
				t.Fatal(err)
			}
			got, err := scanRecord(termsRowFixture{rec, blob})
			if err != nil || got.Kind != rec.Kind || got.UnderlyingID != rec.UnderlyingID || !got.AsOf.Equal(rec.AsOf) || !proto.Equal(got.Terms, rec.Terms) {
				t.Fatalf("consistent indexed/payload fields were refused or normalized: %+v error=%v", got, err)
			}
		})
	}
}

func TestTermsPutRefusesPrecisionLossBeforeDatabaseAccess(t *testing.T) {
	rec := optionRec("OPTION", "UNDERLYING", 100, t0.Add(time.Nanosecond), 1)
	if err := NewPostgres(nil).Put(context.Background(), []Record{rec}); err == nil {
		t.Fatal("unsupported timestamp reached transaction boundary")
	}
}
