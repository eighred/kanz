package natsconfig

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/outbox"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/sustainability"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	"github.com/eighred/kanz/internal/testidentity"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/google/uuid"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Only fixed pinned-server diagnostic-template counters are retained. No raw
// server diagnostic, credential, client address or subject argument is printed.
type filingDenials struct {
	publish             atomic.Uint64
	startupFatal        atomic.Uint64
	jetStreamStartFatal atomic.Uint64
}

func (*filingDenials) Noticef(string, ...any) {}
func (*filingDenials) Warnf(string, ...any)   {}
func (*filingDenials) Debugf(string, ...any)  {}
func (*filingDenials) Tracef(string, ...any)  {}
func (d *filingDenials) Fatalf(format string, _ ...any) {
	d.startupFatal.Add(1)
	if format == "Can't start JetStream: %v" {
		d.jetStreamStartFatal.Add(1)
	}
}
func (d *filingDenials) Errorf(format string, _ ...any) {
	if strings.HasSuffix(format, "Publish Violation - Subject %q") {
		d.publish.Add(1)
	}
}

func TestFilingPublishersUseActualAccountPermissionsAndReturnBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, err := embeddedBaseline()
	if err != nil {
		t.Fatal("embedded account baseline missing")
	}
	// Retain the canonical exact-byte check before either native parser. This
	// fixture never accepts includes/environment/parser capability from a caller.
	document, err := parseBaseline(raw)
	if err != nil {
		t.Fatal("account baseline fingerprint refused")
	}
	accounts, ok := document["accounts"].(map[string]any)
	if !ok || uniqueUsers(accounts) != nil {
		t.Fatal("account baseline identity inventory invalid")
	}
	options := new(ns.Options)
	if err := options.ProcessConfigString(string(raw)); err != nil {
		t.Fatal("actual account configuration refused")
	}
	options.Host = "127.0.0.1"
	options.Port = -1
	options.NoLog = true
	options.NoSigs = true
	options.JetStream = true
	options.StoreDir = t.TempDir()
	options.MaxPayload = 1 << 20
	options.MaxConn = 12
	// Account reservations must fit the server even though the streams below
	// only use bounded memory storage. Preserve the canonical acme 64MB/2GB
	// quota; this disposable server declaration does not alter account policy.
	options.JetStreamMaxMemory = 128 << 20
	options.JetStreamMaxStore = (2 << 30) + (16 << 20)
	domain := spiffeid.RequireTrustDomainFromString("kanz.internal")
	authority, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal("synthetic identity authority refused")
	}
	issue := func(id string) *testidentity.Source {
		t.Helper()
		source, err := authority.Issue(spiffeid.RequireFromString(id), time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal("synthetic SVID issuance failed")
		}
		return source
	}
	const brokerID = "spiffe://kanz.internal/ns/kanz-messaging/sa/nats"
	brokerSource := issue(brokerID)
	options.TLSConfig = tlsconfig.MTLSServerConfig(brokerSource, brokerSource, tlsconfig.AuthorizeMemberOf(domain))
	options.TLSConfig.MinVersion = tls.VersionTLS13
	options.TLSMap = true
	server, err := ns.NewServer(options)
	if err != nil {
		t.Fatal("bounded account broker configuration refused")
	}
	denials := new(filingDenials)
	server.SetLogger(denials, false, false)
	server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(3 * time.Second) {
		t.Fatalf("owned account broker did not become ready: startup_fatal_count=%d jetstream_start_fatal_count=%d", denials.startupFatal.Load(), denials.jetStreamStartFatal.Load())
	}
	url := "tls://" + server.Addr().String()
	tlsFor := func(id string) *tls.Config { return transport.ClientTLSConfig(issue(id), transport.AuthorizeMesh()) }
	connect := func(id string) *nats.Conn {
		t.Helper()
		nc, err := nats.Connect(url, nats.Secure(tlsFor(id)), nats.Timeout(time.Second), nats.NoReconnect(), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if err != nil {
			t.Fatal("actual account SVID was refused")
		}
		t.Cleanup(nc.Close)
		return nc
	}
	admins := map[string]*nats.Conn{
		"__system__": connect("spiffe://kanz.internal/ns/kanz-messaging/sa/nats-bootstrap"),
		"acme":       connect("spiffe://kanz.internal/ns/kanz-services/sa/oms-acme"),
	}
	type streams struct{ risk, compliance jetstream.Stream }
	accountStreams := map[string]streams{}
	var tenantReturn jetstream.Stream
	for _, tenant := range []string{"__system__", "acme"} {
		js, err := jetstream.New(admins[tenant])
		if err != nil {
			t.Fatal(err)
		}
		risk, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "RISK", Subjects: []string{"risk.sustainability.>"}, Storage: jetstream.MemoryStorage, MaxBytes: 1 << 20, MaxMsgs: 32, MaxAge: time.Minute, Duplicates: time.Minute})
		if err != nil {
			t.Fatal("owned RISK stream setup refused")
		}
		compliance, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "COMPLIANCE", Subjects: []string{"compliance.filing.>"}, Storage: jetstream.MemoryStorage, MaxBytes: 1 << 20, MaxMsgs: 32, MaxAge: time.Minute, Duplicates: time.Minute})
		if err != nil {
			t.Fatal("owned COMPLIANCE stream setup refused")
		}
		accountStreams[tenant] = streams{risk, compliance}
		if tenant == "__system__" {
			tenantReturn, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "TENANT_FACT", Subjects: []string{"tfact.acme.risk.sustainability.>", "tfact.acme.compliance.filing.>"}, Storage: jetstream.MemoryStorage, MaxBytes: 1 << 20, MaxMsgs: 32, MaxAge: time.Minute, Duplicates: time.Minute})
			if err != nil {
				t.Fatal("owned tenant return stream setup refused")
			}
		}
	}
	for _, tenant := range []string{"__system__", "acme"} {
		identity := "spiffe://kanz.internal/ns/kanz-services/sa/regulatory"
		if tenant == "acme" {
			identity += "-acme"
		}
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "filing-account-proof", TLSConfig: tlsFor(identity), DisableReconnect: true, PublishTimeout: time.Second})
		if err != nil {
			t.Fatal("actual filing account identity refused")
		}
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Error("owned account producer cleanup failed")
			}
		})
		producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-account-proof", Tenant: tenant})
		if err != nil {
			t.Fatal(err)
		}
		publisher, err := regulatorypublish.New(producer, tenant)
		if err != nil {
			t.Fatal(err)
		}
		bundle := filingAccountBundle(t, tenant)
		for _, event := range bundle {
			record, err := outbox.From(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := record.Event()
			if err != nil {
				t.Fatal(err)
			}
			if err := publisher.Publish(ctx, stored); err != nil {
				t.Fatal("actual filing FACT publish permission/readback failed")
			}
			if err := publisher.Publish(ctx, stored); err != nil {
				t.Fatal("persisted identity retry failed")
			}
		}
		foreign := "acme"
		if tenant == "acme" {
			foreign = "__system__"
		}
		for _, event := range filingAccountBundle(t, foreign) {
			record, err := outbox.From(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := record.Event()
			if err != nil {
				t.Fatal(err)
			}
			if err := publisher.Publish(ctx, stored); err == nil {
				t.Fatal("configured publisher accepted another tenant's otherwise-valid artifact")
			}
		}
		for _, stream := range []jetstream.Stream{accountStreams[tenant].risk, accountStreams[tenant].compliance} {
			info, err := stream.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(3)
			if info.Config.Name == "COMPLIANCE" {
				want = 1
			}
			if info.State.Msgs != want {
				t.Fatal("account publication set missing or reused domain identities")
			}
			for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
				message, err := stream.GetMsg(ctx, sequence)
				if err != nil {
					t.Fatal("actual account FACT missing")
				}
				envelope, _, err := bus.Unframe(message.Data)
				if err != nil || bus.Validate(envelope) != nil || envelope.GetTenantId() != tenant {
					t.Fatal("account readback exposed a foreign tenant or malformed FACT")
				}
			}
		}
		// A real broker refusal, without printing its diagnostic. This is a
		// publish-only role: it cannot halt the platform or administer streams.
		nc := connect(identity)
		for _, subject := range []string{"platform.mode.changed", "$JS.API.STREAM.LIST"} {
			before := denials.publish.Load()
			if err := nc.Publish(subject, []byte(`{}`)); err != nil {
				t.Fatal("denial control transport failed")
			}
			if err := nc.FlushTimeout(time.Second); err != nil {
				t.Fatal("denial control flush failed")
			}
			deadline := time.NewTimer(time.Second)
			tick := time.NewTicker(time.Millisecond)
			for denials.publish.Load() == before {
				select {
				case <-ctx.Done():
					deadline.Stop()
					tick.Stop()
					t.Fatal("denial observation deadline")
				case <-deadline.C:
					tick.Stop()
					t.Fatal("actual server did not deny forbidden filing operation")
				case <-tick.C:
				}
			}
			deadline.Stop()
			tick.Stop()
		}
	}
	// Check both logical streams AFTER both sides have published; checking the
	// platform before acme alone would miss a later unprefixed foreign import.
	for tenant, streams := range accountStreams {
		for _, stream := range []jetstream.Stream{streams.risk, streams.compliance} {
			info, err := stream.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(3)
			if info.Config.Name == "COMPLIANCE" {
				want = 1
			}
			if info.State.Msgs != want {
				t.Fatal("foreign/duplicate publication entered a tenant's logical stream")
			}
			for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
				message, err := stream.GetMsg(ctx, sequence)
				if err != nil {
					t.Fatal(err)
				}
				envelope, _, err := bus.Unframe(message.Data)
				if err != nil || envelope.GetTenantId() != tenant {
					t.Fatal("logical stream contains a foreign tenant envelope")
				}
			}
		}
	}
	// Account isolation prevents an acme publish appearing under the platform's
	// logical subject. Only the configured tfact.acme return import carries it.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		info, err := tenantReturn.Info(ctx)
		if err != nil {
			t.Fatal("tenant return observation failed")
		}
		if info.State.Msgs == 4 {
			for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
				message, err := tenantReturn.GetMsg(ctx, sequence)
				if err != nil {
					t.Fatal(err)
				}
				envelope, _, err := bus.Unframe(message.Data)
				if err != nil || envelope.GetTenantId() != "acme" || !strings.HasPrefix(message.Subject, "tfact.acme.") {
					t.Fatal("tenant return import changed envelope tenant or routing prefix")
				}
			}
			break
		}
		if info.State.Msgs > 4 {
			t.Fatal("tenant return dedup/history count changed")
		}
		select {
		case <-ctx.Done():
			t.Fatal("tenant return deadline")
		case <-deadline.C:
			t.Fatal("actual tenant return FACTs missing")
		case <-tick.C:
		}
	}
}

func filingAccountBundle(t *testing.T, tenant string) []bus.Event {
	t.Helper()
	manifest := []byte(`{"origin":"filing-request/unverified","holding":{"instrument":"synthetic","market_value":"100","scopes":[1,1,2],"revenue":"10","evic":"50","esg":[70,60,80,90]},"scenario":{"name":"synthetic-request","carbon_price":1,"physical_severity":0.1},"glide_path":{"base_year":2020,"target_year":2050,"base_emissions":100},"year":2030,"temperature_sensitivity":2}`)
	digest := sha256.Sum256(manifest)
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	scope := filing.PublicationScope{Tenant: tenant, Portfolio: "pf1", ReportID: id.String(), InputDigest: hex.EncodeToString(digest[:]), AsOf: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), RecordedAt: time.Now().UTC()}
	holding := sustainability.Holding{InstrumentID: "synthetic", MarketValue: 100, ESG: sustainability.ESGScore{Overall: 70, Environmental: 60, Social: 80, Governance: 90}, Carbon: sustainability.CarbonMetrics{Scope1: 1, Scope2: 1, Scope3: 2, Revenue: 10, EVIC: 50}}
	input := sustainability.DisclosureInputs{Holdings: []sustainability.Holding{holding}, GlidePath: sustainability.GlidePath{BaseYear: 2020, TargetYear: 2050, BaseEmissions: 100}, Year: 2030, Scenario: sustainability.ClimateScenario{Name: "synthetic-request", CarbonPrice: 1, PhysicalSeverity: 0.1}, TempSensitivity: 2}
	report, err := sustainability.FileTCFD(input, scope.AsOf, filing.HashSigner{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := regulatorypublish.Report(report, scope.ReportID, &domainpb.MeasureProvenance{Method: string(riskv1.MethodClimateDisclosure), InputManifestDigest: scope.InputDigest})
	if err != nil {
		t.Fatal(err)
	}
	market, ok := dec.ParseProtoExact("100")
	if !ok {
		t.Fatal("invalid fixture market value")
	}
	revenue, ok := dec.ParseProtoExact("10")
	if !ok {
		t.Fatal("invalid fixture revenue")
	}
	enterprise, ok := dec.ParseProtoExact("50")
	if !ok {
		t.Fatal("invalid fixture EVIC")
	}
	observations, aggregate, err := sustainabilitypublish.Build(scope, []sustainabilitypublish.Input{{Holding: holding, MarketValue: market, Revenue: revenue, Enterprise: enterprise, ESGPresent: true}}, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
	if err != nil {
		t.Fatal(err)
	}
	body := &regulatorypb.FilingRecordBody{Report: wire, TenantId: tenant, PortfolioId: scope.Portfolio, InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: scope.InputDigest, ProducerVersion: "synthetic-account-proof", FilingCanonical: report.Canonical(), SignatureKind: regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH, RecordedAt: timestamppb.New(scope.RecordedAt), InstrumentObservations: observations, PortfolioObservation: aggregate, ClimateScenario: &sustainabilitypb.ClimateScenario{Name: input.Scenario.Name, CarbonPrice: input.Scenario.CarbonPrice, PhysicalSeverity: input.Scenario.PhysicalSeverity}}
	canonical, err := regulatorypublish.BindingCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := regulatorypublish.Recorded(body, signer.Link{Prev: chain.Genesis, Cur: chain.Next(chain.Genesis, canonical), Body: canonical})
	if err != nil {
		t.Fatal(err)
	}
	var payloads []proto.Message
	for _, observation := range observations {
		payloads = append(payloads, &sustainabilitypb.InstrumentObserved{Observation: observation, ContextBindingHash: recorded.BindingHash})
	}
	payloads = append(payloads, &sustainabilitypb.PortfolioAggregated{Observation: aggregate, ContextBindingHash: recorded.BindingHash})
	events := make([]bus.Event, 0, 4)
	for _, payload := range payloads {
		event, err := sustainabilitypublish.Event(payload)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	event, err := regulatorypublish.Event(recorded)
	if err != nil {
		t.Fatal(err)
	}
	return append(events, event)
}
