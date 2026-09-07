package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/orderid"
)

// submission is one order this harness put on the wire and the instant it did.
type submission struct {
	id string
	at time.Time
}

// submitter issues orders through the api-gateway's HTTP write surface, and
// through nothing else.
//
// THE FRONT DOOR IS THE POINT, not a convenience. A harness that published an
// order COMMAND straight onto the bus would skip the gateway's authentication,
// its trade-role check, its halt gate and its idempotency handling — four
// controls on the capital path — and would then report a throughput number for a
// path no client can take. AGENTS.md's rule that no step is skippable "not by a
// test helper that just needs a fill" is exactly this case, and
// test/arch/load_harness_front_door_test.go enforces it over whatever lands in
// test/load/ rather than trusting this paragraph.
type submitter struct {
	cfg    config
	client *http.Client
	runTag string
	// accepted counts the submissions the gateway answered 202 to. Read live by
	// runStage's outstanding sampler, so it is atomic rather than guarded by the
	// stage's own mutex.
	accepted atomic.Int64
}

// runTagLen is how much of each order id names THIS run.
//
// Every order id is <runTag><random>, 32 hex characters in total — the shape
// orderid.Mint produces and the only shape OKX accepts as a clOrdId, which is why
// this harness mints ids the same way the gateway does instead of inventing a
// readable one. The tag exists because the FACT stream is SHARED and RETAINED: a
// broker that has been up for a day carries other runs' orders, and a harness
// that counted stream totals would report a number that climbs every time it is
// re-run. Everything here is counted per-id, and every id is one this process
// minted.
const runTagLen = 8

func newSubmitter(cfg config) (*submitter, error) {
	var t [runTagLen / 2]byte
	if _, err := rand.Read(t[:]); err != nil {
		return nil, err
	}
	s := &submitter{
		cfg:    cfg,
		runTag: hex.EncodeToString(t[:]),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// Sized for the offered rate: without this the default 2 idle
				// connections per host make every submission open a new socket, and
				// the run measures TCP setup and ephemeral-port exhaustion on the
				// load generator's own machine.
				MaxIdleConns:        1024,
				MaxIdleConnsPerHost: 1024,
				MaxConnsPerHost:     1024,
			},
		},
	}
	// Fail loudly at construction rather than on the first 400: an id shape the
	// estate refuses would make every submission fail for a reason that has
	// nothing to do with load.
	if err := orderid.Valid(s.mintID()); err != nil {
		return nil, fmt.Errorf("this harness mints an order id the estate refuses: %w", err)
	}
	return s, nil
}

func (s *submitter) mintID() string {
	var b [(orderid.MaxLen - runTagLen) / 2]byte
	_, _ = rand.Read(b[:])
	return s.runTag + hex.EncodeToString(b[:])
}

// order builds the SubmitOrder command body.
//
// A LIMIT ORDER, CARRYING ITS OWN PRICE, and that is a measurement decision
// rather than a default. The pre-trade gate refuses an order it cannot value
// (Unpriced) before it reaches the rule engine, and a MARKET order is valued from
// the mark fold — so on a rig whose price spine is quiet, every market order
// would be refused and the run would measure the refusal path at whatever rate
// the runner can produce it. A limit order carries the price with it, so the gate
// values it, projects the book, and runs the mandate's rules: the work this
// harness exists to put under load.
func (s *submitter) order(id string) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		OrderId:      id,
		PortfolioId:  s.cfg.portfolio,
		InstrumentId: s.cfg.instrument,
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   &commonpb.Decimal{Coefficient: 10_000, Exponent: -2},
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		Venue:        s.cfg.venue,
	}
}

// submitOne places one order and reports the id and the HTTP status.
//
// THE IDEMPOTENCY KEY IS THE ORDER ID. The gateway refuses a submit that carries
// neither (#723), and a key that were fresh per attempt would defeat every dedup
// layer downstream — which is the defect that refusal exists to prevent, and not
// something a load harness may reintroduce for its own convenience.
func (s *submitter) submitOne(ctx context.Context) (submission, int, error) {
	id := s.mintID()
	body, err := protojson.Marshal(s.order(id))
	if err != nil {
		return submission{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.cfg.baseURL, "/")+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		return submission{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Idempotency-Key", id)

	sent := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return submission{id: id, at: sent}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drained so the connection can be reused; a leaked body turns the pool into
	// one connection per request and the run measures sockets.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return submission{id: id, at: sent}, resp.StatusCode, nil
}

// submitAndRead is submitOne plus the response body, for the preflight canary —
// the one place a refusal's REASON matters more than its rate.
func (s *submitter) submitAndRead(ctx context.Context) (submission, int, string, error) {
	id := s.mintID()
	body, err := protojson.Marshal(s.order(id))
	if err != nil {
		return submission{}, 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.cfg.baseURL, "/")+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		return submission{}, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Idempotency-Key", id)

	sent := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return submission{id: id, at: sent}, 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return submission{id: id, at: sent}, resp.StatusCode, strings.TrimSpace(string(raw)), nil
}

// describeStatuses renders the non-202 tally, sorted so two runs are comparable.
// Status 0 is this harness's OWN failed request (a timeout, a refused
// connection, a submission still in flight when the plateau ended) and is
// labelled as such: it is a fact about the generator, and reporting it as a
// server error would send the reader to the wrong process.
func describeStatuses(m map[int]int) string {
	codes := make([]int, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		label := fmt.Sprintf("http %d", c)
		if c == 0 {
			label = "no response (client-side failure or end-of-plateau cancellation)"
		}
		parts = append(parts, fmt.Sprintf("%s x%d", label, m[c]))
	}
	return strings.Join(parts, ", ")
}

// offer runs one plateau: `rate` submissions per second for as long as ctx lives.
//
// The ticker is the OPEN model. Workers take from it; when they cannot keep up
// the tick is counted as a MISSED OFFER rather than queued, because a queued tick
// silently converts the run into a closed model whose measured rate is the
// harness's own service time. verdictFor turns a large shortfall into a stage
// verdict, so a run that measured the generator says so.
func (s *submitter) offer(ctx context.Context, cfg config, rate int, res *stageResult) []submission {
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	workers := rate * 2
	if workers < 8 {
		workers = 8
	}
	if workers > 512 {
		workers = 512
	}
	slots := make(chan struct{}, workers)

	var (
		mu   sync.Mutex
		out  []submission
		wg   sync.WaitGroup
		miss int
	)
	record := func(sub submission, code int, err error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			res.httpOther[0]++
		case code == http.StatusAccepted:
			res.http202++
			s.accepted.Add(1)
			out = append(out, sub)
		default:
			res.httpOther[code]++
		}
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			mu.Lock()
			res.offered = res.http202
			for _, n := range res.httpOther {
				res.offered += n
			}
			if miss > 0 {
				res.notes = append(res.notes, fmt.Sprintf(
					"%d tick(s) found no free submitter and were NOT sent — the generator, not the platform, "+
						"was the limit for those", miss))
			}
			// EVERY NON-202 IS NAMED, not folded into a rate. 429 is the gateway
			// shedding, 423 is the platform halted, 403 is an entitlement, and 0 is
			// this process's own request failing — four different conclusions that a
			// single "error rate" would report as one number.
			if len(res.httpOther) > 0 {
				res.notes = append(res.notes, "non-202 responses: "+describeStatuses(res.httpOther))
			}
			mu.Unlock()
			return out
		case <-ticker.C:
			select {
			case slots <- struct{}{}:
			default:
				mu.Lock()
				miss++
				mu.Unlock()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				sub, code, err := s.submitOne(ctx)
				record(sub, code, err)
			}()
		}
	}
}
