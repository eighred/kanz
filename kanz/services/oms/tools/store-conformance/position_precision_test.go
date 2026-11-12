package storeconformance

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/subject"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	omscomp "github.com/eighred/kanz/services/oms/internal/compliance"
	"github.com/eighred/kanz/services/oms/internal/position"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Both actual owners publish through the durable outbox and broker. Observe
// quantities/value independently, then feed the PostgreSQL snapshot into the
// production pre-trade BookSource. Risk ingest is qualified in its own module.
func TestRustPositionPrecisionPostgresNATS(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			f := newFixture(t)
			address := os.Getenv("TEST_NATS_URL")
			if address == "" {
				t.Skip("TEST_NATS_URL unavailable: actual precision broker required")
			}
			nc, err := nats.Connect(address, nats.Timeout(5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			js, err := jetstream.New(nc)
			if err != nil {
				t.Fatal(err)
			}
			portfolio := f.schema
			instrument := fill().InstrumentId
			key := subject.PositionFor(f.tenant, portfolio, instrument)
			stream, err := bustest.StreamFor(f.ctx, js, key)
			if err != nil {
				t.Fatal(err)
			}
			name := "precision-" + f.schema
			consumer, err := js.CreateConsumer(f.ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: key, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := js.DeleteConsumer(ctx, stream, name); err != nil {
					t.Error(err)
				}
			})
			for i := int64(1); i <= 11; i++ {
				when := at.Add(time.Duration(i) * time.Second)
				v := positionFill(fmt.Sprint(i), orderpb.Side_SIDE_BUY, 1, 1, when)
				v.Quantity.Exponent, v.Price.Exponent = -9, -9
				qty, value, realized := fmt.Sprintf("%d/1000000000", i), fmt.Sprintf("%d/1000000000000000000", i), "0"
				if i == 11 {
					v.Side, v.Quantity.Coefficient, v.Price.Coefficient = orderpb.Side_SIDE_SELL, 12, 2
					qty, value, realized = "-2/1000000000", "-4/1000000000000000000", "1/100000000000000000"
				}
				var applied, duplicate *position.Applied
				if owner == "rust" {
					applied = positionResult(t, runResult(t, positionRequest(t, f, portfolio, v, when, true)))
					duplicate = positionResult(t, runResult(t, positionRequest(t, f, portfolio, v, when, false)))
				} else {
					book := position.NewPostgres(f.open(f.tenant), "USD")
					applied, err = book.Apply(f.ctx, portfolio, v, when, func(_ context.Context, a *position.Applied) ([]outbox.Record, error) {
						return positionRecords(t, f.tenant, portfolio, v, a), nil
					})
					if err != nil {
						t.Fatal(err)
					}
					duplicate, err = position.NewPostgres(f.open(f.tenant), "USD").Apply(f.ctx, portfolio, v, when, nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				positionParity(t, duplicate, applied)
				positionNumbers(t, applied.Aggregate, qty, "0", realized, value, "0")
				var journal []byte
				if err := f.pool.QueryRow(f.ctx, "SELECT fill FROM position_execution_history WHERE raw_fill_id=$1", v.FillId).Scan(&journal); err != nil || !bytes.Equal(journal, wire(t, v)) {
					t.Fatal("raw exact execution changed", err)
				}
				pending := pendingRelay(t, f, key)
				if len(pending) != 2 {
					t.Fatal("missing atomic precision announcements")
				}
				since := time.Now()
				relayResult(t, runResult(t, relayRequest(f, "flush", key, 8)), 2, "")
				frame := verifyRelayFrame(t, pending[0].Record, (relayBroker{consumer: consumer}).next(t, key), 1, since)
				var observed domainpb.PositionState
				if proto.Unmarshal(frame.Payload, &observed) != nil {
					t.Fatal("invalid broker position")
				}
				positionNumbers(t, &observed, qty, "0", realized, value, "0")
				wantQty, _ := new(big.Rat).SetString(qty)
				wantValue, _ := new(big.Rat).SetString(value)
				// A newly opened Go reader must observe the Rust writer too.
				book, err := omscomp.NewBookSource(position.NewPostgres(f.open(f.tenant), "USD"), nil, nil, nil).Book(f.ctx, portfolio)
				if err != nil || len(book.Positions) != 1 {
					t.Fatal("control book unavailable", err)
				}
				if positionRat(book.Positions[0].Quantity).Cmp(wantQty) != 0 {
					t.Fatal("control snapshot rounded holding")
				}
				deny := &compliancepb.Rule{Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT, Mode: compliancepb.RestrictionMode_RESTRICTION_MODE_DENY, Values: []string{instrument}}}}
				if comp.RestrictionRule(&comp.Candidate{Book: book}, deny) == nil {
					t.Fatal("tiny marked holding disappeared from restriction enforcement")
				}
				result := runResult(t, map[string]any{"mode": "position-snapshot", "schema": f.schema, "tenant": f.tenant, "portfolio_id": portfolio, "at": clock(when), "base_currency": "USD"})
				expect(t, result.Status, "snapshot")
				var snap domainpb.PortfolioSnapshot
				if proto.Unmarshal(result.State, &snap) != nil || len(snap.Positions) != 1 {
					t.Fatal("Rust restarted snapshot missing")
				}
				if positionRat(snap.Positions[0].Quantity).Cmp(wantQty) != 0 || positionRat(snap.Portfolio.TotalMarketValue.Amount).Cmp(wantValue) != 0 {
					t.Fatal("Rust snapshot lost exact quantity/value")
				}
			}
		})
	}
}

func TestRustPositionPrecisionTransactions(t *testing.T) {
	for _, owner := range []string{"go", "rust"} {
		t.Run(owner, func(t *testing.T) {
			t.Run("finite-overflow-rolls-back", func(t *testing.T) {
				f := newFixture(t)
				first := positionFill("first", orderpb.Side_SIDE_BUY, 9223372036854775807, 1, at)
				apply := func(v *orderpb.Fill, refuse bool) {
					t.Helper()
					if owner == "rust" {
						r := runResult(t, positionRequest(t, f, "portfolio", v, v.ExecutedAt.AsTime(), true))
						if refuse {
							expect(t, r.Status, "Capacity")
							if len(r.Aggregate) != 0 || len(r.Venue) != 0 {
								t.Fatal("refusal exposed candidate")
							}
						} else {
							positionResult(t, r)
						}
					} else {
						_, err := position.NewPostgres(f.open(f.tenant), "USD").Apply(f.ctx, "portfolio", v, v.ExecutedAt.AsTime(), func(_ context.Context, a *position.Applied) ([]outbox.Record, error) {
							return positionRecords(t, f.tenant, "portfolio", v, a), nil
						})
						if (err != nil) != refuse {
							t.Fatal("wrong finite representability outcome", err)
						}
					}
				}
				apply(first, false)
				before := positionDurable(t, f)
				v := positionFill("overflow", orderpb.Side_SIDE_BUY, 1, 1, at.Add(time.Second))
				apply(v, true)
				if !reflect.DeepEqual(before, positionDurable(t, f)) {
					t.Fatal("refusal changed claim/history/lot/outbox")
				}
				// Quantity fits, but its finite mark cannot fit the canonical coefficient.
				v.Price.Coefficient = 2
				v.Side = orderpb.Side_SIDE_SELL
				apply(v, true)
				if !reflect.DeepEqual(before, positionDurable(t, f)) {
					t.Fatal("finite money refusal changed durable evidence")
				}
			})
			t.Run("repeating-cost-keeps-executions", func(t *testing.T) {
				f := newFixture(t)
				for i, c := range []struct {
					side orderpb.Side
					q, p int64
				}{{orderpb.Side_SIDE_BUY, 1, 1}, {orderpb.Side_SIDE_BUY, 2, 2}, {orderpb.Side_SIDE_SELL, 1, 2}} {
					v := positionFill(fmt.Sprint(i), c.side, c.q, c.p, at.Add(time.Duration(i)*time.Second))
					var a *position.Applied
					if owner == "rust" {
						a = positionResult(t, runResult(t, positionRequest(t, f, "portfolio", v, v.ExecutedAt.AsTime(), true)))
					} else {
						var err error
						a, err = position.NewPostgres(f.open(f.tenant), "USD").Apply(f.ctx, "portfolio", v, v.ExecutedAt.AsTime(), nil)
						if err != nil {
							t.Fatal(err)
						}
					}
					if i == 2 {
						positionNumbers(t, a.Aggregate, "2", "166666667/100000000", "33333333/100000000", "4", "66666667/100000000")
					}
				}
				var count int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_execution_history").Scan(&count) != nil || count != 3 {
					t.Fatal("derived rounding discarded execution")
				}
			})
			t.Run("tiny-repeating-cost-refuses-snapshot", func(t *testing.T) {
				f := newFixture(t)
				for i, c := range []struct {
					side orderpb.Side
					q, p int64
				}{{orderpb.Side_SIDE_BUY, 1, 1}, {orderpb.Side_SIDE_BUY, 2, 2}, {orderpb.Side_SIDE_SELL, 1, 2}} {
					v := positionFill(fmt.Sprint(i), c.side, c.q, c.p, at.Add(time.Duration(i)*time.Second))
					v.Quantity.Exponent, v.Price.Exponent = -9, -9
					if owner == "rust" {
						positionResult(t, runResult(t, positionRequest(t, f, "portfolio", v, v.ExecutedAt.AsTime(), true)))
					} else {
						if _, err := position.NewPostgres(f.open(f.tenant), "USD").Apply(f.ctx, "portfolio", v, v.ExecutedAt.AsTime(), nil); err != nil {
							t.Fatal(err)
						}
					}
				}
				before := positionDurable(t, f)
				if snap, err := position.NewPostgres(f.open(f.tenant), "USD").Snapshot(f.ctx, "portfolio", at); err == nil || snap != nil {
					t.Fatal("Go served a rounded-flat snapshot")
				}
				result := runResult(t, map[string]any{"mode": "position-snapshot", "schema": f.schema, "tenant": f.tenant, "portfolio_id": "portfolio", "at": clock(at), "base_currency": "USD"})
				expect(t, result.Status, "Capacity")
				if len(result.State) != 0 {
					t.Fatal("Rust exposed a refused snapshot")
				}
				if book, err := omscomp.NewBookSource(position.NewPostgres(f.open(f.tenant), "USD"), nil, nil, nil).Book(f.ctx, "portfolio"); err == nil || book != nil {
					t.Fatal("control read admitted an unavailable cost mark")
				}
				if !reflect.DeepEqual(before, positionDurable(t, f)) {
					t.Fatal("read refusal rewrote committed executions")
				}
				var count int
				if f.pool.QueryRow(f.ctx, "SELECT count(*) FROM position_execution_history").Scan(&count) != nil || count != 3 {
					t.Fatal("snapshot failure discarded executions")
				}
			})
		})
	}
}
