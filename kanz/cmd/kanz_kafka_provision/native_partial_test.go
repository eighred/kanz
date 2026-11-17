package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eighred/kanz/cmd/kanz_kafka_provision/internal/provision"
	"github.com/segmentio/kafka-go"
)

type nativePartialRecords struct {
	reader   kafka.RecordReader
	closeErr error
	closed   int
}

func (r *nativePartialRecords) ReadRecord() (*kafka.Record, error) { return r.reader.ReadRecord() }
func (r *nativePartialRecords) Close() error                       { r.closed++; return r.closeErr }

type nativePartialReadError struct{ err error }

func (r nativePartialReadError) ReadRecord() (*kafka.Record, error) { return nil, r.err }

type nativePartialBytes struct {
	closed int
	err    error
}

func (b *nativePartialBytes) Read([]byte) (int, error) { return 0, io.EOF }
func (b *nativePartialBytes) Len() int                 { return 0 }
func (b *nativePartialBytes) Close() error             { b.closed++; return b.err }

var _ kafka.Bytes = (*nativePartialBytes)(nil)

type nativePartialFake struct {
	t                  *testing.T
	ctx                context.Context
	fetches, writes    int
	responses          []func(*kafka.FetchRequest) (*kafka.FetchResponse, error)
	produceErr         error
	produceResponseErr error
}

func (f *nativePartialFake) Fetch(ctx context.Context, req *kafka.FetchRequest) (*kafka.FetchResponse, error) {
	f.t.Helper()
	if ctx != f.ctx || f.writes != 0 || req.Addr != nil || req.Topic != "partial.order.order" || req.Partition != 0 || req.Offset != 0 || req.MinBytes != 1 || req.MaxBytes != 8192 || req.MaxWait != time.Second {
		f.t.Fatal("readiness changed context, routing, bounds or write ordering")
	}
	index := f.fetches
	f.fetches++
	if index >= len(f.responses) {
		index = len(f.responses) - 1
	}
	return f.responses[index](req)
}

func (f *nativePartialFake) Produce(ctx context.Context, req *kafka.ProduceRequest) (*kafka.ProduceResponse, error) {
	f.t.Helper()
	f.writes++
	if ctx != f.ctx || f.writes != 1 || req.Addr != nil || req.Topic != "partial.order.order" || req.Partition != 0 || req.RequiredAcks != kafka.RequireAll {
		f.t.Fatal("sentinel write was retried, rerouted or changed its contract")
	}
	record, err := req.Records.ReadRecord()
	if err != nil || record == nil {
		f.t.Fatal("sentinel record missing")
	}
	value, err := kafka.ReadAll(record.Value)
	if err != nil || string(value) != "preserved-partial-history" {
		f.t.Fatal("sentinel changed")
	}
	if err := record.Value.Close(); err != nil {
		f.t.Fatal("sentinel value close failed")
	}
	if _, err := req.Records.ReadRecord(); !errors.Is(err, io.EOF) {
		f.t.Fatal("extra sentinel write")
	}
	return &kafka.ProduceResponse{Error: f.produceResponseErr}, f.produceErr
}

func emptyPartialResponse(records *nativePartialRecords) func(*kafka.FetchRequest) (*kafka.FetchResponse, error) {
	return func(req *kafka.FetchRequest) (*kafka.FetchResponse, error) {
		return &kafka.FetchResponse{Topic: req.Topic, Partition: 0, Records: records}, nil
	}
}

func TestNativePartialFetchReadinessPrecedesOneSentinelWrite(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		callErr, responseErr error
	}{
		{"success", nil, nil},
		{"produce-call-error", kafka.NotLeaderForPartition, nil},
		{"produce-response-error", nil, kafka.NotLeaderForPartition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			records := &nativePartialRecords{reader: kafka.NewRecordReader()}
			f := &nativePartialFake{t: t, ctx: ctx, produceErr: tc.callErr, produceResponseErr: tc.responseErr, responses: []func(*kafka.FetchRequest) (*kafka.FetchResponse, error){
				func(*kafka.FetchRequest) (*kafka.FetchResponse, error) { return nil, kafka.NotLeaderForPartition },
				func(req *kafka.FetchRequest) (*kafka.FetchResponse, error) {
					return &kafka.FetchResponse{Topic: req.Topic, Error: kafka.LeaderNotAvailable, Records: kafka.NewRecordReader()}, nil
				},
				emptyPartialResponse(records),
			}}
			diagnostic := &nativeDiagnostic{Phase: "partial-ready"}
			err := nativePartialProduce(ctx, f, "partial.order.order", diagnostic)
			cancel()
			wantErr := tc.callErr
			if wantErr == nil {
				wantErr = tc.responseErr
			}
			if !errors.Is(err, wantErr) || f.fetches != 3 || f.writes != 1 || records.closed != 1 || diagnostic.Phase != "partial-produce" {
				t.Fatalf("readiness/write sequence changed: %v", err)
			}
		})
	}
}

func TestNativePartialFetchReadinessRefusesUnknownAndClosesRecords(t *testing.T) {
	for _, name := range []string{"nil-response", "wrong-topic", "wrong-partition", "auth-call", "auth-response", "transport", "nil-records", "read-error", "nil-progress", "unexpected-record", "record-close-error", "close-error", "leader-close-error"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			records := &nativePartialRecords{reader: kafka.NewRecordReader()}
			key, value := &nativePartialBytes{}, &nativePartialBytes{}
			response := &kafka.FetchResponse{Topic: "partial.order.order", Records: records}
			var callErr error
			want := error(provision.ErrBroker)
			switch name {
			case "nil-response":
				response = nil
			case "wrong-topic":
				response.Topic = "foreign.order.order"
				response.Error = kafka.LeaderNotAvailable
				want = provision.ErrConflict
			case "wrong-partition":
				response.Partition = 1
				callErr = kafka.NotLeaderForPartition
				want = provision.ErrConflict
			case "auth-call":
				callErr = kafka.TopicAuthorizationFailed
				want = callErr
			case "auth-response":
				response.Error = kafka.TopicAuthorizationFailed
				want = response.Error
			case "transport":
				callErr = io.ErrUnexpectedEOF
				want = callErr
			case "nil-records":
				response.Records = nil
			case "read-error":
				records.reader = nativePartialReadError{io.ErrUnexpectedEOF}
				want = io.ErrUnexpectedEOF
			case "nil-progress":
				records.reader = nativePartialReadError{}
			case "unexpected-record", "record-close-error":
				records.reader = kafka.NewRecordReader(kafka.Record{Key: key, Value: value})
				want = provision.ErrConflict
			case "close-error":
				records.closeErr = io.ErrClosedPipe
			case "leader-close-error":
				response.Error = kafka.NotLeaderForPartition
				records.closeErr = io.ErrClosedPipe
			}
			if name == "record-close-error" {
				value.err = io.ErrClosedPipe
				want = provision.ErrBroker
			}
			f := &nativePartialFake{t: t, ctx: ctx, responses: []func(*kafka.FetchRequest) (*kafka.FetchResponse, error){func(*kafka.FetchRequest) (*kafka.FetchResponse, error) { return response, callErr }}}
			diagnostic := &nativeDiagnostic{Phase: "partial-ready"}
			if err := nativePartialProduce(ctx, f, "partial.order.order", diagnostic); !errors.Is(err, want) || f.fetches != 1 || f.writes != 0 || diagnostic.Phase != "partial-ready" {
				t.Fatalf("unknown response retried or wrote: %v", err)
			}
			wantClosed := 1
			if response == nil || name == "nil-records" {
				wantClosed = 0
			}
			if records.closed != wantClosed {
				t.Fatal("response records were not closed exactly once")
			}
			if (name == "unexpected-record" || name == "record-close-error") && (key.closed != 1 || value.closed != 1) {
				t.Fatal("unexpected record fields were not both closed")
			}
		})
	}
}

func TestNativePartialFetchReadinessHonorsOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		f := &nativePartialFake{t: t, ctx: ctx, responses: []func(*kafka.FetchRequest) (*kafka.FetchResponse, error){func(*kafka.FetchRequest) (*kafka.FetchResponse, error) { return nil, kafka.NotLeaderForPartition }}}
		if err := nativePartialProduce(ctx, f, "partial.order.order", &nativeDiagnostic{Phase: "partial-ready"}); !errors.Is(err, context.DeadlineExceeded) || f.fetches != 1 || f.writes != 0 {
			t.Fatalf("persistent leader transition escaped deadline: %v", err)
		}
		cancelled, stop := context.WithCancel(context.Background())
		stop()
		f.ctx = cancelled
		f.fetches = 0
		if err := nativePartialProduce(cancelled, f, "partial.order.order", &nativeDiagnostic{Phase: "partial-ready"}); !errors.Is(err, context.Canceled) || f.fetches != 0 || f.writes != 0 {
			t.Fatal("cancelled readiness launched traffic")
		}
		ready, cancelReady := context.WithCancel(context.Background())
		defer cancelReady()
		records := &nativePartialRecords{reader: kafka.NewRecordReader()}
		f = &nativePartialFake{t: t, ctx: ready, responses: []func(*kafka.FetchRequest) (*kafka.FetchResponse, error){func(req *kafka.FetchRequest) (*kafka.FetchResponse, error) {
			cancelReady()
			return emptyPartialResponse(records)(req)
		}}}
		if err := nativePartialProduce(ready, f, "partial.order.order", &nativeDiagnostic{Phase: "partial-ready"}); !errors.Is(err, context.Canceled) || f.fetches != 1 || f.writes != 0 || records.closed != 1 {
			t.Fatal("cancelled successful read authorized a write")
		}
	})
}

func TestNativePartialEmptyReadinessIsNotSentinelEvidence(t *testing.T) {
	ctx := context.Background()
	records := &nativePartialRecords{reader: kafka.NewRecordReader()}
	readback := &nativePartialRecords{reader: kafka.NewRecordReader()}
	f := &nativePartialFake{t: t, ctx: ctx, responses: []func(*kafka.FetchRequest) (*kafka.FetchResponse, error){emptyPartialResponse(records), emptyPartialResponse(readback)}}
	if err := nativePartialFetchReady(ctx, f, "partial.order.order"); err != nil {
		t.Fatal(err)
	}
	if err := nativeReadContains(ctx, f, "partial.order.order", "preserved-partial-history"); !errors.Is(err, provision.ErrMissing) || f.writes != 0 || records.closed != 1 || readback.closed != 1 {
		t.Fatal("empty readiness became sentinel evidence")
	}
}
