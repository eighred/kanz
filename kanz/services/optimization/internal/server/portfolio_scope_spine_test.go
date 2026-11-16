package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/optimization"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type proposalScopeTransport struct {
	*bus.NATSClient
	created chan [2]string
}

func (c *proposalScopeTransport) SubscribeBroadcastObserved(ctx context.Context, filter string, h bus.Handler, ready func(), created bus.ConsumerCreated) error {
	return c.NATSClient.SubscribeBroadcastObserved(ctx, filter, h, ready, func(stream, consumer string) {
		if created != nil {
			created(stream, consumer)
		}
		c.created <- [2]string{stream, consumer}
	})
}

func TestProposalPortfolioScopeCanonicalMandateReplay(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires canonical MANDATE NATS topology")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	id := fmt.Sprintf("proposal-scope-%d", time.Now().UnixNano())
	tenant, otherTenant := id+"-a", id+"-b"
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "MANDATE")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Config.MaxAge != 0 || info.Config.MaxMsgsPerSubject != 1 {
		t.Fatal("canonical compacted never-ageing MANDATE required", err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	topics := []string{compliance.SubjectMandateFor(tenant, "PF-A"), compliance.SubjectMandateFor(tenant, "PF-B"), compliance.SubjectMandateFor(otherTenant, "PF-A")}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, topic := range topics {
			if strings.ContainsAny(topic, "*>") {
				t.Error("unsafe mandate fixture cleanup")
				continue
			}
			if err := stream.Purge(cleanup, jetstream.WithPurgeSubject(topic)); err != nil {
				t.Error(err)
			}
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "kanz-mandate", ProducerVersion: "proposal-scope-test"})
	if err != nil {
		t.Fatal(err)
	}
	publisher := compliance.NewPublisher(producer, client)
	pub := func(m *compliancepb.Mandate) {
		t.Helper()
		digest, err := compliance.MandateDigest(m, id)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		proposal, err := dualcontrol.Propose(id+"-"+m.TenantId+"-"+m.PortfolioId, dualcontrol.ActMandateChange, "mandate", "operator:maker", digest, now, dualcontrol.DefaultTTL)
		if err != nil {
			t.Fatal(err)
		}
		approval, err := proposal.Approve("operator:checker", digest, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := publisher.Publish(ctx, m, approval, id); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	a, b, foreign := concentrationMandate(tenant, "PF-A", 100), concentrationMandate(tenant, "PF-B", 40), concentrationMandate(otherTenant, "PF-A", 40)
	for _, m := range []*compliancepb.Mandate{a, b, foreign} {
		m.EffectiveAt = timestamppb.New(now.Add(-time.Minute))
		pub(m)
	}
	retained := func(topic string) *jetstream.RawStreamMsg {
		t.Helper()
		m, err := stream.GetLastMsgForSubject(ctx, topic)
		if err != nil {
			t.Fatal(err)
		}
		env, payload, err := bus.Unframe(m.Data)
		var cc lifecyclepb.ConfigChanged
		if err != nil || bus.Validate(env) != nil || env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || env.EventType != compliance.EventTypeMandateChanged || proto.Unmarshal(payload, &cc) != nil || cc.ChangedBy == cc.ApprovedBy || cc.ChangedBy == "" || cc.ApprovedBy == "" {
			t.Fatal("not a canonical independently approved mandate", err)
		}
		return m
	}
	before := map[string][]byte{}
	for _, topic := range topics {
		before[topic] = bytes.Clone(retained(topic).Data)
	}
	type session struct {
		source   *proposalScopeSource
		consumer jetstream.Consumer
		endpoint *httptest.Server
		stop     func()
	}
	start := func() session {
		t.Helper()
		reg := compliance.NewMandateRegistry()
		transport := &proposalScopeTransport{client, make(chan [2]string, 1)}
		consumer, err := bus.NewConsumer(transport)
		if err != nil {
			t.Fatal(err)
		}
		subCtx, stop := context.WithCancel(ctx)
		ready, done := make(chan struct{}), make(chan error, 1)
		var once sync.Once
		var owned [2]string
		join := func() {
			once.Do(func() {
				stop()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("mandate broadcast did not join")
				}
				if owned[1] != "" {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := js.DeleteConsumer(cleanup, owned[0], owned[1]); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						t.Error(err)
					}
				}
			})
		}
		t.Cleanup(join)
		go func() {
			defer close(done)
			done <- consumer.SubscribeBroadcastReady(subCtx, compliance.SubjectMandateAll, compliance.NewMandateConsumer(reg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handle, func() { reg.Arm(); close(ready) })
		}()
		select {
		case owned = <-transport.created:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-ready:
		case err := <-done:
			t.Fatal("broadcast ended before readiness", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		c, err := js.Consumer(ctx, owned[0], owned[1])
		if err != nil {
			t.Fatal(err)
		}
		src := &proposalScopeSource{source: reg}
		s := gatedServer(t, src)
		s.materializerFor = func(string) Materializer { t.Error("read-only proposal reached order publication"); return nil }
		endpoint := httptest.NewServer(s)
		t.Cleanup(endpoint.Close)
		return session{src, c, endpoint, join}
	}
	awaitACK := func(s session, sequence uint64) {
		t.Helper()
		for {
			info, err := s.consumer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumPending == 0 && info.NumAckPending == 0 && info.AckFloor.Stream >= sequence {
				return
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("canonical mandate was not ACKed", ctx.Err())
			}
		}
	}
	live := start()
	awaitACK(live, retained(topics[2]).Sequence)
	request := func(t *testing.T, s session, who, pf string, claims []string, want int, verdict optimization.MandateStatus) {
		t.Helper()
		calls := s.source.calls.Load()
		status, data := scopeProposalResponse(t, s.endpoint.Client(), scopeProposalRequest(t, s.endpoint.URL, who, pf, claims, "pm"))
		reads := int64(1)
		if want == 403 {
			reads = 0
		}
		if status != want || s.source.calls.Load()-calls != reads {
			t.Fatalf("scope status=%d reads=%d data=%s", status, s.source.calls.Load()-calls, data)
		}
		if want == 200 {
			requireScopeExactProposal(t, data, pf, verdict)
		} else if strings.Contains(string(data), "single-name-cap") {
			t.Fatal("foreign limit disclosed")
		}
	}
	t.Run("warm_authorized", func(t *testing.T) {
		request(t, live, tenant, "PF-A", []string{"PF-A"}, 200, optimization.MandateFeasible)
	})
	t.Run("warm_denied", func(t *testing.T) {
		request(t, live, tenant, "PF-B", []string{"PF-A"}, 403, optimization.MandateUnchecked)
	})
	t.Run("warm_authorized_other", func(t *testing.T) {
		request(t, live, tenant, "PF-B", []string{"PF-B"}, 200, optimization.MandateInfeasible)
	})
	t.Run("revoked", func(t *testing.T) {
		request(t, live, tenant, "PF-B", []string{"PF-A"}, 403, optimization.MandateUnchecked)
		for topic, expected := range before {
			if !bytes.Equal(retained(topic).Data, expected) {
				t.Fatal("read changed authoritative mandate")
			}
		}
	})
	t.Run("other_tenant", func(t *testing.T) {
		request(t, live, otherTenant, "PF-A", []string{"PF-A"}, 200, optimization.MandateInfeasible)
	})
	t.Run("live_update", func(t *testing.T) {
		updated := proto.Clone(b).(*compliancepb.Mandate)
		updated.Version = 2
		updated.EffectiveAt = timestamppb.New(now.Add(-10 * time.Second))
		updated.Rules[0].GetConcentration().MaxWeight.Coefficient = 100
		pub(updated)
		awaitACK(live, retained(topics[1]).Sequence)
		request(t, live, tenant, "PF-B", []string{"PF-B"}, 200, optimization.MandateFeasible)
	})
	live.endpoint.Close()
	live.stop()
	cold := start()
	awaitACK(cold, retained(topics[1]).Sequence)
	t.Run("restart_authorized", func(t *testing.T) {
		request(t, cold, tenant, "PF-B", []string{"PF-B"}, 200, optimization.MandateFeasible)
	})
	t.Run("restart_denied", func(t *testing.T) {
		request(t, cold, tenant, "PF-B", []string{"PF-A"}, 403, optimization.MandateUnchecked)
	})
	t.Run("restart_other_tenant", func(t *testing.T) {
		request(t, cold, otherTenant, "PF-A", []string{"PF-A"}, 200, optimization.MandateInfeasible)
	})
	cold.endpoint.Close()
	cold.stop()
}
