// Command depth-conformance exports Go Binance/parser/book compatibility data.
// All HTTP and websocket traffic terminates at an ephemeral loopback fixture.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const snapshotMS int64 = 1700000000000

type step struct {
	Kind          string `json:"kind"`
	Wire          []int  `json:"wire"`
	BookWire      []int  `json:"bookWire"`
	QuoteEligible bool   `json:"quoteEligible"`
}
type fixture struct {
	Name      string   `json:"name"`
	Snapshots []string `json:"snapshots"`
	Events    []string `json:"events"`
	Steps     []step   `json:"steps"`
	Eligible  []bool   `json:"-"`
}
type corpus struct {
	Version        int       `json:"version"`
	SnapshotUnixMS int64     `json:"snapshotUnixMs"`
	Cases          []fixture `json:"cases"`
}

func fixtures() []fixture {
	return []fixture{
		{Name: "numeric-sort-duplicates-delete", Snapshots: []string{`{"lastUpdateId":100,"bids":[["2","1"],["10","2"],["10.0","3"],["9","4"],["2","0"]],"asks":[["100","2"],["11","3"],["20","4"]]}`}, Events: []string{
			`{"E":1700000000001,"U":101,"u":101,"b":[["10.00","0"],["2.0","5"],["9","-1"]],"a":[["11","0"],["12","2"],["12.0","7"]]}`,
			`{"E":1700000000002,"U":102,"u":102,"b":[],"a":[["12","0"],["20","0"],["100","0"]]}`,
		}, Eligible: []bool{true, true, false}},
		{Name: "rounding-rescale-invalid", Snapshots: []string{`{"lastUpdateId":200,"bids":[["0.0000000049","1"],["0.000000005","2"],["-0.000000005","3"],["1.234567895","4"],["92233720368.54775807","5"],["92233720368.54775808","6"],["1000000000000","1000000000000"],["1e100","1"],["bad","2"],["3","bad"],["4"],[],["5","6","ignored"]],"asks":[["2000000000000","1"]]}`}, Eligible: []bool{false}},
		{Name: "stale-straddle-gap-reanchor", Snapshots: []string{
			`{"lastUpdateId":100,"bids":[["10","1"]],"asks":[["11","1"]]}`,
			`{"lastUpdateId":110,"bids":[["20","1"]],"asks":[["21","1"]]}`,
		}, Events: []string{
			`{"E":1700000000001,"U":90,"u":99,"b":[["99","1"]]}`,
			`{"E":1700000000002,"U":99,"u":102,"b":[["10","2"]]}`,
			`{"E":1700000000003,"U":104,"u":105,"b":[["98","1"]]}`,
			`{"E":1700000000004,"U":109,"u":111,"a":[["21","2"]]}`,
		}, Eligible: []bool{true, true, true, true}},
		{Name: "locked-crossed-nonpositive", Snapshots: []string{`{"lastUpdateId":300,"bids":[["10","1"]],"asks":[["10","1"]]}`}, Events: []string{
			`{"E":1700000000001,"U":301,"u":301,"b":[["11","1"]]}`,
			`{"E":1700000000002,"U":302,"u":302,"b":[["11","0"],["10","0"],["0","1"]]}`,
		}, Eligible: []bool{false, false, false}},
	}
}

func wire(m proto.Message) ([]int, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out, nil
}

func evaluate(f *fixture) error {
	var mu sync.Mutex
	restCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if restCalls >= len(f.Snapshots) {
			http.Error(w, "unexpected snapshot", 500)
			return
		}
		_, _ = io.WriteString(w, f.Snapshots[restCalls])
		restCalls++
	})
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := conn.CloseRead(r.Context())
		for _, event := range f.Events {
			if conn.Write(ctx, websocket.MessageText, []byte(event)) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	source := depth.NewBinanceSource(depth.BinanceConfig{InstrumentID: "BTC-USD", Symbol: "BTCUSDT", MIC: "BINANCE", WSBase: srv.URL, RESTBase: srv.URL, HTTPClient: srv.Client()})
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := book.New("BTC-USD", "BTCUSDT", "BINANCE")
	for _, eligible := range f.Eligible {
		u, err := source.Recv(ctx)
		if err != nil {
			return err
		}
		s := step{QuoteEligible: eligible}
		if u.Snapshot != nil {
			u.Snapshot.EventTime = timestamppb.New(time.UnixMilli(snapshotMS))
			s.Kind = "snapshot"
			s.Wire, err = wire(u.Snapshot)
			if err == nil {
				err = b.ApplySnapshot(u.Snapshot)
			}
		} else {
			s.Kind = "delta"
			s.Wire, err = wire(u.Delta)
			if err == nil {
				err = b.ApplyDelta(u.Delta)
			}
		}
		if err != nil {
			return err
		}
		s.BookWire, err = wire(b.Snapshot(0))
		if err != nil {
			return err
		}
		f.Steps = append(f.Steps, s)
	}
	mu.Lock()
	defer mu.Unlock()
	if restCalls != len(f.Snapshots) {
		return fmt.Errorf("snapshot calls %d, want %d", restCalls, len(f.Snapshots))
	}
	return nil
}

func run(w io.Writer) error {
	c := corpus{Version: 1, SnapshotUnixMS: snapshotMS, Cases: fixtures()}
	for i := range c.Cases {
		if err := evaluate(&c.Cases[i]); err != nil {
			return fmt.Errorf("%s: %w", c.Cases[i].Name, err)
		}
	}
	return json.NewEncoder(w).Encode(c)
}
func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
