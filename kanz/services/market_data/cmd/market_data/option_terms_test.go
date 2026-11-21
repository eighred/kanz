package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/marketdatatest"
	"github.com/eighred/kanz/internal/marketdata/terms"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestOptionTermsReadUsesStoredRevisionAndBothCutoffs(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL for restricted PostgreSQL")
	}
	ctx := t.Context()
	migrations := filepath.Join("..", "..", "migrations")
	pool := marketdatatest.NewPool(t, ctx, dsn, migrations)
	ddl, err := os.ReadFile(filepath.Join(migrations, "0002_contract_terms.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MARKET_DATA_DATABASE_URL_FILE", "")
	t.Setenv("MARKET_DATA_DATABASE_URL", pool.Config().ConnConfig.ConnString())
	t.Setenv("PGOPTIONS", "-c default_transaction_read_only=on")

	store := terms.NewPostgres(pool)
	effective := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	const id = "TEST-OPT"
	contract := func(at time.Time, strike int64) terms.Record {
		option := &referencepb.OptionTerms{
			UnderlyingId: "TEST-UND", Strike: &commonpb.Decimal{Coefficient: strike},
			Expiry:     timestamppb.New(effective.AddDate(1, 0, 0)),
			OptionType: referencepb.OptionType_OPTION_TYPE_CALL, ExerciseStyle: referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN,
			ContractMultiplier: &commonpb.Decimal{Coefficient: 25},
		}
		return terms.Record{
			InstrumentID: id, UnderlyingID: option.UnderlyingId, Kind: terms.KindOption, AsOf: at,
			Terms: &referencepb.ContractTerms{InstrumentId: id, AsOf: timestamppb.New(at), Terms: &referencepb.ContractTerms_Option{Option: option}},
		}
	}
	appendRecord := func(r terms.Record) (time.Time, string) {
		t.Helper()
		if err := store.Put(ctx, []terms.Record{r}); err != nil {
			t.Fatal(err)
		}
		var stored time.Time
		var wire []byte
		if err := pool.QueryRow(ctx, "SELECT ingested_at, terms FROM contract_terms WHERE instrument_id=$1 AND as_of=$2", r.InstrumentID, r.AsOf).Scan(&stored, &wire); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(wire)
		return stored, hex.EncodeToString(digest[:])
	}
	read := func(instrument string, at, by time.Time, pin string) ([]byte, error) {
		args := []string{"--instrument", instrument, "--effective-at", at.Format(time.RFC3339Nano), "--ingested-by", by.Format(time.RFC3339Nano)}
		if pin != "" {
			args = append(args, "--sha256", pin)
		}
		var out bytes.Buffer
		err := readOptionTerms(args, &out)
		return out.Bytes(), err
	}
	original := contract(effective, 85)
	firstKnown, firstPin := appendRecord(original)
	amendment := contract(effective.Add(time.Hour), 90)
	secondKnown, secondPin := appendRecord(amendment)
	if !secondKnown.After(firstKnown) {
		t.Fatalf("ingestion timestamps did not advance: %v %v", firstKnown, secondKnown)
	}

	check := func(at, by time.Time, expect terms.Record, known time.Time, pin string) []byte {
		t.Helper()
		output, err := read(id, at, by, pin)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			IngestedAt time.Time `json:"ingested_at"`
			SHA256     string
			Contract   json.RawMessage
		}
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		got := new(referencepb.ContractTerms)
		if err := protojson.Unmarshal(result.Contract, got); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, expect.Terms) || result.SHA256 != pin || !known.Equal(result.IngestedAt) {
			t.Fatalf("wrong retained version: %s", output)
		}
		return output
	}
	earlier := check(amendment.AsOf, firstKnown, original, firstKnown, firstPin)
	check(original.AsOf, secondKnown, original, firstKnown, firstPin)
	check(amendment.AsOf, secondKnown, amendment, secondKnown, secondPin)
	if got := check(amendment.AsOf, firstKnown, original, firstKnown, firstPin); !bytes.Equal(got, earlier) {
		t.Fatal("pinned replay changed bytes")
	}
	for _, tc := range []struct {
		id     string
		at, by time.Time
	}{
		{id, effective.Add(-time.Microsecond), secondKnown},
		{id, amendment.AsOf, firstKnown.Add(-time.Microsecond)},
		{"UNLISTED", amendment.AsOf, secondKnown},
	} {
		got, err := read(tc.id, tc.at, tc.by, "")
		if !errors.Is(err, terms.ErrNoTerms) || len(got) != 0 {
			t.Fatalf("unexpected missing revision outcome %q, %v", got, err)
		}
	}
	if got, err := read(id, amendment.AsOf, secondKnown, firstPin); err == nil || len(got) != 0 {
		t.Fatal("mismatched pin exposed a result")
	}
	changed := terms.Record{InstrumentID: id, AsOf: amendment.AsOf.Add(time.Hour), Kind: terms.KindSwap}
	changed.Terms = &referencepb.ContractTerms{InstrumentId: id, AsOf: timestamppb.New(changed.AsOf), Terms: &referencepb.ContractTerms_Swap{Swap: &referencepb.SwapTerms{}}}
	thirdKnown, _ := appendRecord(changed)
	if out, err := read(id, changed.AsOf, thirdKnown, ""); !errors.Is(err, terms.ErrNotOption) || len(out) != 0 {
		t.Fatalf("reclassified contract returned stale option: %s, %v", out, err)
	}
	if out, err := read(id, amendment.AsOf, secondKnown, strings.Repeat("z", 64)); err == nil || len(out) != 0 {
		t.Fatal("malformed digest accepted")
	}
}

func TestOptionTermsCutoffPrecision(t *testing.T) {
	for _, raw := range []string{"", "2020-03-01", "2020-03-01T00:00:00.0000001Z", "2020-03-01T00:00:00.0000000001Z"} {
		if _, err := parseTermsCutoff(raw, "--effective-at"); err == nil {
			t.Fatalf("accepted invalid cutoff %q", raw)
		}
	}
	if _, err := parseTermsCutoff("2020-03-01T00:00:00.123456+03:00", "--effective-at"); err != nil {
		t.Fatal(err)
	}
}
