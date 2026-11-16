package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/coder/websocket"
	"github.com/eighred/kanz/internal/marketedge/book"
	"github.com/eighred/kanz/internal/marketedge/depth"
)

// The positive-sequence corpus preserves actual Go semantics. Zero sequence
// resets are separately tested on the replacement runtime: the legacy Go book
// treats delta sequence zero as omitted, so it is not an oracle for that fix.
func runOKX(w io.Writer) error {
	c := corpus{Version: 1, SnapshotUnixMS: snapshotMS, Cases: []fixture{
		{Name: "okx-sort-equivalent-delete", Events: []string{
			`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"snapshot","data":[{"bids":[["2","1","0","1"],["10","2","0","1"],["10.0","3","0","1"]],"asks":[["20","2","0","1"],["11","3","0","1"]],"ts":"1700000000000","seqId":100,"prevSeqId":-1}]}`,
			`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"update","data":[{"bids":[["10.00","0","0","1"],["2.0","5","0","1"]],"asks":[["11","0","0","1"],["12","2","0","1"],["12.0","7","0","1"]],"ts":"1700000000001","seqId":101,"prevSeqId":100}]}`,
			`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"update","data":[{"bids":[],"asks":[["12","0","0","1"],["20","0","0","1"]],"ts":"1700000000002","seqId":102,"prevSeqId":101}]}`,
		}, Eligible: []bool{true, true, false}},
		{Name: "okx-rounding-rescale", Events: []string{
			`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"snapshot","data":[{"bids":[["0.000000005","1.234567895","0","1"],["92233720368.54775808","1000000000000","0","1"]],"asks":[["2000000000000","2","0","1"]],"ts":"1700000000000","seqId":200,"prevSeqId":-1}]}`,
		}, Eligible: []bool{true}},
	}}
	for i := range c.Cases {
		if err := evaluateOKX(&c.Cases[i]); err != nil {
			return fmt.Errorf("%s: %w", c.Cases[i].Name, err)
		}
	}
	return json.NewEncoder(w).Encode(c)
}

func evaluateOKX(f *fixture) error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err = conn.Read(r.Context()); err != nil {
			return
		}
		ctx := conn.CloseRead(r.Context())
		for _, frame := range f.Events {
			if conn.Write(ctx, websocket.MessageText, []byte(frame)) != nil {
				return
			}
		}
		<-ctx.Done()
	}))
	defer srv.Close()
	source := depth.NewOKXSource(depth.OKXConfig{InstrumentID: "BTC-USD", InstID: "BTC-USDT", MIC: "OKX", WSURL: srv.URL, HTTPClient: srv.Client()})
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := book.New("BTC-USD", "BTC-USDT", "OKX")
	for _, eligible := range f.Eligible {
		u, err := source.Recv(ctx)
		if err != nil {
			return err
		}
		s := step{QuoteEligible: eligible}
		if u.Snapshot != nil {
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
	return nil
}
