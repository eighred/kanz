package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const handoffCIDBudget uint64 = 4096
const handoffMaxClients = 16 // The disposable server's explicit MaxConn.

// The configured SPIFFE verifier authenticates the peer during TLS. This only
// extracts that completed TLS identity, not the client-supplied NATS name or
// proof that the later CONNECT authorization has completed.
func handoffTLSPrincipal(state *tls.ConnectionState) (string, bool) {
	if state == nil || !state.HandshakeComplete || state.Version != tls.VersionTLS13 ||
		len(state.PeerCertificates) != 1 || state.PeerCertificates[0] == nil {
		return "", false
	}
	cert := state.PeerCertificates[0]
	if len(cert.URIs) != 1 || cert.URIs[0] == nil {
		return "", false
	}
	id, err := spiffeid.FromURI(cert.URIs[0])
	if err != nil || id.TrustDomain().String() != "kanz.internal" {
		return "", false
	}
	return id.String(), true
}

// A CID in both collections cannot have disconnected and been reused. Every
// common CID therefore spanned the midpoint. Its count must exclude any other
// registered CID there. nil represents an UNKNOWN collection, unlike known empty.
func handoffPrincipalSnapshot(first map[uint64]string, midpoint int, second map[uint64]string) bool {
	if first == nil || second == nil || midpoint < 0 || midpoint > handoffMaxClients ||
		len(first) != midpoint || len(second) != midpoint {
		return false
	}
	for cid, peer := range first {
		if cid == 0 || cid > handoffCIDBudget || peer == "" || second[cid] != peer {
			return false
		}
	}
	return true
}

type handoffObservation struct {
	deadline time.Time
	count    int
}

func (o *handoffObservation) take(now time.Time) bool {
	if !now.Before(o.deadline) || o.count >= 10000 {
		return false
	}
	o.count++
	return true
}

// Only the immediately preceding fully validated Rust payload is repeatable.
// Envelope publication identity may change; venue time and payload may not.
type handoffPrevious struct {
	frame    *envelopepb.EventFrame
	sequence uint64
	producer uint64
}

func (p *handoffPrevious) classify(f *envelopepb.EventFrame, actual, target uint64, now, deadline time.Time) (bool, error) {
	e := f.GetEnvelope()
	if !now.Before(deadline) || e == nil || e.ProducerSequence == 0 || e.ProducerSequence <= p.producer {
		return false, errors.New("observation order or deadline refused")
	}
	if actual != target {
		if p.frame == nil || actual != p.sequence || !bytes.Equal(f.Payload, p.frame.Payload) || !proto.Equal(e.EventTime, p.frame.GetEnvelope().EventTime) {
			return false, errors.New("non-immediate or altered repeat refused")
		}
		p.producer = e.ProducerSequence
		return true, nil
	}
	p.producer = e.ProducerSequence
	return false, nil
}

func (p *handoffPrevious) remember(f *envelopepb.EventFrame, sequence uint64) {
	p.frame = proto.Clone(f).(*envelopepb.EventFrame)
	p.sequence = sequence
}

func TestFeedHandoffExactRepeatControl(t *testing.T) {
	principal := "spiffe://kanz.internal/ns/test/sa/feed"
	observer := "spiffe://kanz.internal/ns/test/sa/observer"
	for _, tc := range []struct {
		name          string
		first, second map[uint64]string
		midpoint      int
		want          bool
	}{
		{"PrincipalInventoryIdentical", map[uint64]string{1: observer, 2: principal}, map[uint64]string{1: observer, 2: principal}, 2, true},
		{"PrincipalInventoryPairedCountABA", map[uint64]string{1: observer, 2: principal}, map[uint64]string{1: observer, 3: principal}, 2, false},
		{"PrincipalInventoryMissedCID", map[uint64]string{1: observer}, map[uint64]string{1: observer}, 2, false},
		{"PrincipalInventoryAboveBudget", map[uint64]string{handoffCIDBudget + 1: principal}, map[uint64]string{handoffCIDBudget + 1: principal}, 1, false},
		{"PrincipalInventoryChangedURI", map[uint64]string{2: principal}, map[uint64]string{2: observer}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if handoffPrincipalSnapshot(tc.first, tc.midpoint, tc.second) != tc.want {
				t.Fatal("coherent principal snapshot outcome differs")
			}
		})
	}
	t.Run("PrincipalInventoryUnknownTLS", func(t *testing.T) {
		// These synthetic states check extraction/refusal only. Actual SVID
		// verification and broker authorization remain in the process rehearsal.
		uri := spiffeid.RequireFromString(principal).URL()
		valid := func() *tls.ConnectionState {
			return &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS13,
				PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{uri}}}}
		}
		if got, ok := handoffTLSPrincipal(valid()); !ok || got != principal {
			t.Fatal("completed TLS-state identity extraction refused")
		}
		states := []*tls.ConnectionState{nil}
		for _, mutate := range []func(*tls.ConnectionState){
			func(s *tls.ConnectionState) { s.HandshakeComplete = false },
			func(s *tls.ConnectionState) { s.Version = tls.VersionTLS12 },
			func(s *tls.ConnectionState) { s.PeerCertificates = nil },
			func(s *tls.ConnectionState) { s.PeerCertificates[0] = nil },
			func(s *tls.ConnectionState) { s.PeerCertificates[0].URIs = nil },
			func(s *tls.ConnectionState) { s.PeerCertificates[0].URIs = []*url.URL{nil} },
			func(s *tls.ConnectionState) { s.PeerCertificates[0].URIs = []*url.URL{uri, uri} },
			func(s *tls.ConnectionState) {
				s.PeerCertificates[0].URIs = []*url.URL{spiffeid.RequireFromString("spiffe://foreign.test/peer").URL()}
			},
		} {
			state := valid()
			mutate(state)
			states = append(states, state)
		}
		for _, state := range states {
			if _, ok := handoffTLSPrincipal(state); ok {
				t.Fatal("unknown TLS became a known principal")
			}
		}
		known := map[uint64]string{2: principal}
		if handoffPrincipalSnapshot(nil, 1, known) || handoffPrincipalSnapshot(known, 1, nil) ||
			handoffPrincipalSnapshot(map[uint64]string{2: ""}, 1, map[uint64]string{2: ""}) {
			t.Fatal("unknown collection became successful absence")
		}
		if !handoffPrincipalSnapshot(map[uint64]string{}, 0, map[uint64]string{}) {
			t.Fatal("known empty snapshot confused with UNKNOWN")
		}
	})
	now := time.Unix(100, 0)
	deadline := now.Add(10 * time.Second)
	frame := func(payload string, producer uint64) *envelopepb.EventFrame {
		return &envelopepb.EventFrame{Payload: []byte(payload), Envelope: &envelopepb.Envelope{ProducerSequence: producer, EventTime: timestamppb.New(now)}}
	}
	previous := frame("validated-price50000-size4", 1)
	fresh := func() handoffPrevious {
		return handoffPrevious{frame: proto.Clone(previous).(*envelopepb.EventFrame), sequence: 101, producer: 1}
	}
	p := fresh()
	if repeat, err := p.classify(frame(string(previous.Payload), 2), 101, 102, now, deadline); err != nil || !repeat {
		t.Fatal("exact repeat refused")
	}
	target := frame("validated-next-state", 3)
	if repeat, err := p.classify(target, 102, 102, now, deadline); err != nil || repeat {
		t.Fatal("new target refused")
	}
	p.remember(target, 102)
	if _, err := p.classify(frame(string(previous.Payload), 4), 101, 105, now, deadline); err == nil {
		t.Fatal("non-immediate history accepted")
	}
	for _, mutate := range []func(*envelopepb.EventFrame){
		func(f *envelopepb.EventFrame) { f.Payload = []byte("validated-price1-size4") },
		func(f *envelopepb.EventFrame) { f.Payload = append(f.Payload, 0) },
		func(f *envelopepb.EventFrame) { f.Envelope.EventTime = timestamppb.New(now.Add(time.Nanosecond)) },
		func(f *envelopepb.EventFrame) { f.Envelope.ProducerSequence = 1 },
		func(f *envelopepb.EventFrame) { f.Envelope.ProducerSequence = 0 },
	} {
		p = fresh()
		f := frame(string(previous.Payload), 2)
		mutate(f)
		if _, err := p.classify(f, 101, 102, now, deadline); err == nil {
			t.Fatal("altered or regressing repeat accepted")
		}
	}
	p = fresh()
	p.producer = 3
	if _, err := p.classify(frame(string(previous.Payload), 2), 101, 102, now, deadline); err == nil {
		t.Fatal("positive regressing producer sequence accepted")
	}
	empty := handoffPrevious{}
	if _, err := empty.classify(previous, 101, 102, now, deadline); err == nil {
		t.Fatal("unproved initial repeat accepted")
	}
	p = fresh()
	if _, err := p.classify(target, 102, 102, deadline, deadline); err == nil {
		t.Fatal("ready target accepted at deadline")
	}
	budget := handoffObservation{deadline: deadline}
	for i := 0; i < 10000; i++ {
		if !budget.take(now) {
			t.Fatal("bounded inventory refused early")
		}
	}
	if budget.take(now) {
		t.Fatal("inventory overflow accepted")
	}
	budget = handoffObservation{deadline: deadline}
	if !budget.take(now) || budget.take(deadline) || !budget.deadline.Equal(deadline) {
		t.Fatal("observation deadline reset or ignored")
	}
}
