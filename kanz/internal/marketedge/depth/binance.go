package depth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/internal/exchange/netdial"
)

// BinanceSource is the live Binance Spot L2 depth feed — a DepthSource over the
// <symbol>@depth diff stream plus the REST depth snapshot, compiled only under
// -tags binance so the default binary links no websocket.
//
// It implements Binance's documented local-order-book procedure, which exists
// because the diff stream and the REST snapshot are not synchronised:
//
//  1. Open the diff stream first and let events queue in the socket buffer.
//  2. Fetch the REST snapshot (its lastUpdateId is the anchor).
//  3. Discard every buffered event whose final id u <= lastUpdateId — it predates
//     the snapshot and is already folded into it.
//  4. The first event applied must STRADDLE the anchor: U <= lastUpdateId+1 <= u.
//     If the first surviving event starts after that, updates were lost between
//     the snapshot and the stream ⇒ re-snapshot.
//  5. Thereafter each event must chain exactly: U == previous u + 1. Anything else
//     is a gap ⇒ re-snapshot.
//
// Re-snapshotting rather than folding out of order is the whole point: a silently
// mis-folded book prices every downstream decision wrong. The source owns its own
// resequencing (per the DepthSource contract), so the ingester's book never sees
// an out-of-order delta.
//
// Sequences are mapped onto the transport-neutral chain the book checks:
// prev_update_sequence is set to the sequence the book is expected to be AT, so
// the straddling first event and the exact-chain steady state both express as one
// rule (`prev == book.lastSeq`).
type BinanceSource struct {
	instrumentID string
	symbol       string // venue symbol, e.g. BTCUSDT
	mic          string
	wsBase       string
	restBase     string
	limit        int
	httpc        *http.Client

	conn    *websocket.Conn
	lastSeq uint64 // last final-id (u) folded; 0 before the first snapshot
	// needSnap forces the next Recv to (re)anchor on a REST snapshot.
	needSnap bool
	// firstAfterSnap relaxes the chain rule for exactly one event — the one that
	// must straddle the snapshot anchor.
	firstAfterSnap bool
}

// BinanceConfig configures a BinanceSource.
type BinanceConfig struct {
	InstrumentID string // canonical Kanz instrument_id (the partition key)
	Symbol       string // venue symbol (BTCUSDT)
	MIC          string // venue code (default "BINANCE")
	WSBase       string // websocket origin, e.g. wss://stream.binance.com:9443
	RESTBase     string // REST origin, e.g. https://api.binance.com
	Limit        int    // REST snapshot depth (<=0 ⇒ 1000)
	DNSTTL       time.Duration
	HTTPClient   *http.Client // injected by tests; default is the DNS-bypass client
}

// NewBinanceSource builds the live depth source.
func NewBinanceSource(cfg BinanceConfig) *BinanceSource {
	if cfg.MIC == "" {
		cfg.MIC = "BINANCE"
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 1000
	}
	if cfg.HTTPClient == nil {
		// Zero-Timeout client: the depth stream is long-lived (see netdial).
		cfg.HTTPClient = netdial.NewWebsocketHTTPClient(cfg.DNSTTL)
	}
	return &BinanceSource{
		instrumentID: cfg.InstrumentID, symbol: cfg.Symbol, mic: cfg.MIC,
		wsBase: strings.TrimSuffix(cfg.WSBase, "/"), restBase: strings.TrimSuffix(cfg.RESTBase, "/"),
		limit: cfg.Limit, httpc: cfg.HTTPClient,
	}
}

var _ DepthSource = (*BinanceSource)(nil)

// binanceDepthEvent is one <symbol>@depth diff-stream frame.
type binanceDepthEvent struct {
	Event   string     `json:"e"`
	EventMS int64      `json:"E"`
	Symbol  string     `json:"s"`
	FirstID uint64     `json:"U"` // first update id in this event
	FinalID uint64     `json:"u"` // final update id in this event
	Bids    [][]string `json:"b"`
	Asks    [][]string `json:"a"`
}

// binanceDepthSnapshot is GET /api/v3/depth.
type binanceDepthSnapshot struct {
	LastUpdateID uint64     `json:"lastUpdateId"`
	Bids         [][]string `json:"bids"`
	Asks         [][]string `json:"asks"`
	Code         int        `json:"code"`
	Msg          string     `json:"msg"`
}

// Recv yields the next depth update, re-anchoring on a REST snapshot whenever the
// venue sequence chain breaks. It never returns a delta the book cannot fold in
// order.
func (s *BinanceSource) Recv(ctx context.Context) (Update, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Update{}, err
		}
		if s.conn == nil {
			// Open the stream BEFORE snapshotting so no update is lost in the gap
			// between the two (step 1 of the venue's procedure).
			if err := s.connect(ctx); err != nil {
				return Update{}, err
			}
			s.needSnap = true
		}
		if s.needSnap {
			snap, err := s.snapshot(ctx)
			if err != nil {
				return Update{}, err
			}
			s.lastSeq = snap.LastUpdateID
			s.needSnap, s.firstAfterSnap = false, true
			return Update{Snapshot: s.snapshotProto(snap)}, nil
		}

		ev, err := s.readEvent(ctx)
		if err != nil {
			return Update{}, err
		}
		switch {
		case ev.FinalID <= s.lastSeq:
			continue // wholly predates the anchor — already in the snapshot
		case s.firstAfterSnap:
			if ev.FirstID > s.lastSeq+1 {
				// Updates were lost between the snapshot and the stream.
				s.needSnap = true
				continue
			}
			s.firstAfterSnap = false
		case ev.FirstID != s.lastSeq+1:
			// A hole in the live chain. Re-anchor rather than fold out of order.
			s.needSnap = true
			continue
		}

		prev := s.lastSeq
		s.lastSeq = ev.FinalID
		return Update{Delta: s.deltaProto(ev, prev)}, nil
	}
}

// connect dials the diff-depth stream. @100ms is the fast cadence — this is the
// hot path, and the frames never leave the process.
func (s *BinanceSource) connect(ctx context.Context) error {
	stream := strings.ToLower(s.symbol) + "@depth@100ms"
	conn, _, err := websocket.Dial(ctx, s.wsBase+"/ws/"+stream, &websocket.DialOptions{HTTPClient: s.httpc})
	if err != nil {
		return fmt.Errorf("binance depth: dial %s: %w", stream, err)
	}
	conn.SetReadLimit(4 << 20) // a 1000-level book is well under this
	s.conn = conn
	return nil
}

// Close releases the stream.
func (s *BinanceSource) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close(websocket.StatusNormalClosure, "")
	s.conn = nil
	return err
}

func (s *BinanceSource) readEvent(ctx context.Context) (*binanceDepthEvent, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		s.conn = nil // force a reconnect + re-anchor on the next Recv
		return nil, fmt.Errorf("binance depth: read: %w", err)
	}
	var ev binanceDepthEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("binance depth: decode event: %w", err)
	}
	return &ev, nil
}

// snapshot fetches the REST depth anchor.
func (s *BinanceSource) snapshot(ctx context.Context) (*binanceDepthSnapshot, error) {
	u := s.restBase + "/api/v3/depth?" + url.Values{
		"symbol": {s.symbol}, "limit": {strconv.Itoa(s.limit)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance depth: snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var snap binanceDepthSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("binance depth: decode snapshot: %w", err)
	}
	if snap.Code != 0 {
		return nil, fmt.Errorf("binance depth: snapshot error %d: %s", snap.Code, snap.Msg)
	}
	if snap.LastUpdateID == 0 {
		return nil, fmt.Errorf("binance depth: snapshot carries no lastUpdateId")
	}
	return &snap, nil
}

func (s *BinanceSource) snapshotProto(snap *binanceDepthSnapshot) *marketpb.OrderBookSnapshot {
	return &marketpb.OrderBookSnapshot{
		InstrumentId:       s.instrumentID,
		Symbol:             s.symbol,
		Mic:                s.mic,
		EventTime:          timestamppb.New(time.Now().UTC()),
		LastUpdateSequence: snap.LastUpdateID,
		Bids:               binanceLevels(snap.Bids),
		Asks:               binanceLevels(snap.Asks),
	}
}

func (s *BinanceSource) deltaProto(ev *binanceDepthEvent, prev uint64) *marketpb.OrderBookDelta {
	return &marketpb.OrderBookDelta{
		InstrumentId:        s.instrumentID,
		Symbol:              s.symbol,
		Mic:                 s.mic,
		EventTime:           timestamppb.New(time.UnixMilli(ev.EventMS).UTC()),
		FirstUpdateSequence: ev.FirstID,
		LastUpdateSequence:  ev.FinalID,
		PrevUpdateSequence:  prev,
		Bids:                binanceLevels(ev.Bids),
		Asks:                binanceLevels(ev.Asks),
	}
}

// binanceLevels converts ["price","size"] pairs to exact PriceLevels. A size of 0
// is preserved — it is the venue's "remove this level" instruction, not noise.
// Prices/sizes are parsed as exact big.Rat: no float ever touches depth.
func binanceLevels(raw [][]string) []*marketpb.PriceLevel {
	out := make([]*marketpb.PriceLevel, 0, len(raw))
	for _, lvl := range raw {
		if len(lvl) < 2 {
			continue
		}
		price, okP := new(big.Rat).SetString(lvl[0])
		size, okS := new(big.Rat).SetString(lvl[1])
		if !okP || !okS {
			continue // unparseable level — drop it rather than guess a price
		}
		out = append(out, &marketpb.PriceLevel{Price: dec.ToProto(price), Size: dec.ToProto(size)})
	}
	return out
}
