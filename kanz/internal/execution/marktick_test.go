package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// tickSink is a Publisher whose answer the test controls, so a REFUSED publish
// can be observed without a broker. It is the case that matters: fakeBus and
// every in-memory capture in this repo accept everything, which is precisely why
// a discarded publish error survived in two services.
type tickSink struct {
	events []bus.Event
	err    error
}

func (s *tickSink) Publish(_ context.Context, e bus.Event) error {
	s.events = append(s.events, e)
	return s.err
}

func markPrice(t *testing.T, v string) *commonpb.Decimal {
	t.Helper()
	r, ok := new(big.Rat).SetString(v)
	if !ok {
		t.Fatalf("bad test price %q", v)
	}
	return dec.ToProto(r)
}

// warnLogger returns a logger writing JSON records into buf.
func warnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// warnRecords returns the decoded WARN records buf holds.
func warnRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// The FACT both venue adapters put on the wire is built HERE, once. If this
// drifts, two exchanges' marks drift with it.
func TestMarkTickPublisher_PublishesTheMarketTradeFact(t *testing.T) {
	sink := &tickSink{}
	p := NewMarkTickPublisher(sink, warnLogger(&bytes.Buffer{}), "BINANCE", nil)

	p.PublishTrade(context.Background(), "BTC-USD", "BTCUSDT", markPrice(t, "50123.5"))

	if len(sink.events) != 1 {
		t.Fatalf("published %d events, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Subject != SubjectMarketCryptoTrade {
		t.Errorf("subject = %q, want %q", e.Subject, SubjectMarketCryptoTrade)
	}
	if e.EventType != SubjectMarketCryptoTrade {
		t.Errorf("event type = %q, want %q", e.EventType, SubjectMarketCryptoTrade)
	}
	if e.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT — a mark is an observation, never a command", e.EventClass)
	}
	if e.Domain != "market" {
		t.Errorf("domain = %q, want market", e.Domain)
	}
	if e.PartitionKey != "BTC-USD" {
		t.Errorf("partition key = %q, want the instrument id — ordering per instrument is what a fold depends on", e.PartitionKey)
	}
	ev, ok := e.Payload.(*marketpb.MarketDataEvent)
	if !ok {
		t.Fatalf("payload is %T, want *marketpb.MarketDataEvent", e.Payload)
	}
	if ev.GetInstrumentId() != "BTC-USD" || ev.GetSymbol() != "BTCUSDT" || ev.GetMic() != "BINANCE" {
		t.Errorf("identity = (%q,%q,%q), want (BTC-USD,BTCUSDT,BINANCE)", ev.GetInstrumentId(), ev.GetSymbol(), ev.GetMic())
	}
	if got := dec.FromProto(ev.GetTrade().GetPrice()); got.Cmp(big.NewRat(1002470, 20)) != 0 {
		t.Errorf("price = %s, want 50123.5", got.RatString())
	}
}

// THE DEFECT ITSELF (#673). A refused publish must move a counter EVERY time —
// this is what an alert is written over, and what tells a refused subject apart
// from an exchange with nothing to say.
func TestMarkTickPublisher_CountsEveryDroppedTick(t *testing.T) {
	sink := &tickSink{err: errors.New("envelope validation: tenant_id required")}
	var dropped []string
	p := NewMarkTickPublisher(sink, warnLogger(&bytes.Buffer{}), "OKX", func(mic, instrumentID string) {
		dropped = append(dropped, mic+"/"+instrumentID)
	})

	for i := 0; i < 3; i++ {
		p.PublishTrade(context.Background(), "BTC-USD", "BTC-USDT", markPrice(t, "1"))
	}
	p.PublishTrade(context.Background(), "ETH-USD", "ETH-USDT", markPrice(t, "2"))

	want := []string{"OKX/BTC-USD", "OKX/BTC-USD", "OKX/BTC-USD", "OKX/ETH-USD"}
	if len(dropped) != len(want) {
		t.Fatalf("observer fired %d times (%v), want %d — the counter is the alertable half and it must "+
			"fire on EVERY drop, not once per instrument like the WARN", len(dropped), dropped, len(want))
	}
	for i := range want {
		if dropped[i] != want[i] {
			t.Fatalf("observation %d = %q, want %q — the mic and instrument are what let an operator tell "+
				"a broker outage from one dead symbol", i, dropped[i], want[i])
		}
	}
}

// THE REASON IS THE WHOLE POINT. "The venue went quiet", "the connection
// dropped" and "the broker refused the envelope" are three different repairs,
// and the error is the only value that separates them — it was the one being
// discarded.
func TestMarkTickPublisher_WarnsOncePerOutageAndCarriesTheReason(t *testing.T) {
	var buf bytes.Buffer
	sink := &tickSink{err: errors.New("envelope validation: tenant_id required")}
	p := NewMarkTickPublisher(sink, warnLogger(&buf), "BINANCE", nil)

	for i := 0; i < 4; i++ {
		p.PublishTrade(context.Background(), "BTC-USD", "BTCUSDT", markPrice(t, "1"))
	}

	recs := warnRecords(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("wrote %d WARN lines for one outage, want exactly 1 — a five-second ticker logging per "+
			"tick buries the log on the day it matters", len(recs))
	}
	rec := recs[0]
	if got, _ := rec["err"].(string); !strings.Contains(got, "tenant_id required") {
		t.Errorf("WARN err = %q, want the broker's own reason — without it a refused envelope and a dead "+
			"connection are the same line", got)
	}
	for _, field := range []string{"mic", "instrument_id", "symbol", "subject", "fix"} {
		if _, ok := rec[field]; !ok {
			t.Errorf("WARN carries no %q — %v", field, rec)
		}
	}
	if got, _ := rec["instrument_id"].(string); got != "BTC-USD" {
		t.Errorf("WARN instrument_id = %q, want BTC-USD", got)
	}
}

// A mark feed recovers; an ungoverned portfolio does not. So this latch clears
// on success, and the SECOND outage is audible — unlike compliance's
// once-per-key-forever latch, which would have made every later outage silent.
func TestMarkTickPublisher_WarnsAgainAfterTheFeedRecovers(t *testing.T) {
	var buf bytes.Buffer
	sink := &tickSink{err: errors.New("nats: no responders")}
	p := NewMarkTickPublisher(sink, warnLogger(&buf), "BINANCE", nil)
	ctx := context.Background()

	p.PublishTrade(ctx, "BTC-USD", "BTCUSDT", markPrice(t, "1")) // outage 1
	p.PublishTrade(ctx, "BTC-USD", "BTCUSDT", markPrice(t, "1")) // still outage 1: silent
	sink.err = nil
	p.PublishTrade(ctx, "BTC-USD", "BTCUSDT", markPrice(t, "1")) // recovery: silent
	sink.err = errors.New("nats: no responders")
	p.PublishTrade(ctx, "BTC-USD", "BTCUSDT", markPrice(t, "1")) // outage 2: audible again

	if got := len(warnRecords(t, &buf)); got != 2 {
		t.Fatalf("wrote %d WARN lines across two outages separated by a recovery, want 2 — a latch that "+
			"never clears makes every outage after the first silent", got)
	}
}

// A per-instrument latch, not a global one: one dead symbol must not silence the
// warning for the next.
func TestMarkTickPublisher_LatchIsPerInstrument(t *testing.T) {
	var buf bytes.Buffer
	sink := &tickSink{err: errors.New("broker refused")}
	p := NewMarkTickPublisher(sink, warnLogger(&buf), "OKX", nil)
	ctx := context.Background()

	p.PublishTrade(ctx, "BTC-USD", "BTC-USDT", markPrice(t, "1"))
	p.PublishTrade(ctx, "ETH-USD", "ETH-USDT", markPrice(t, "2"))
	p.PublishTrade(ctx, "BTC-USD", "BTC-USDT", markPrice(t, "1"))

	recs := warnRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("wrote %d WARN lines for two failing instruments, want 2", len(recs))
	}
	seen := map[string]bool{}
	for _, r := range recs {
		s, _ := r["instrument_id"].(string)
		seen[s] = true
	}
	if !seen["BTC-USD"] || !seen["ETH-USD"] {
		t.Fatalf("WARNs named %v, want both instruments — a shared latch hides the second symbol", seen)
	}
}

// A nil observer is a degraded posture, not a crash: the WARN still fires. The
// completeness guard in test/arch is what keeps a composition root from choosing
// it silently.
func TestMarkTickPublisher_NilObserverStillWarns(t *testing.T) {
	var buf bytes.Buffer
	p := NewMarkTickPublisher(&tickSink{err: errors.New("boom")}, warnLogger(&buf), "OKX", nil)
	p.PublishTrade(context.Background(), "BTC-USD", "BTC-USDT", markPrice(t, "1"))
	if got := len(warnRecords(t, &buf)); got != 1 {
		t.Fatalf("wrote %d WARN lines with a nil observer, want 1", got)
	}
}

// A successful tick says nothing and counts nothing. A publisher that logged per
// tick would be its own outage.
func TestMarkTickPublisher_SuccessIsSilent(t *testing.T) {
	var buf bytes.Buffer
	fired := 0
	p := NewMarkTickPublisher(&tickSink{}, warnLogger(&buf), "BINANCE", func(string, string) { fired++ })
	p.PublishTrade(context.Background(), "BTC-USD", "BTCUSDT", markPrice(t, "1"))
	if buf.Len() != 0 {
		t.Errorf("a landed tick logged %q", buf.String())
	}
	if fired != 0 {
		t.Errorf("a landed tick fired the dropped observer %d times", fired)
	}
}
