package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/regulatory"
	riskv1 "github.com/eighred/kanz/internal/risk/api/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func syntheticRecord(t *testing.T) *regulatorypb.FilingRecorded {
	t.Helper()
	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	manifest := []byte(`{"version":1,"origin":"filing-request/unverified","GrossAssetValue":"1000","NetAssetValue":"900","VaR":"5","GrossExposure":"1200"}`)
	digest := sha256.Sum256(manifest)
	input := hex.EncodeToString(digest[:])
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	report, err := regulatory.FileFormPF(regulatory.FormPFInputs{GrossAssetValue: big.NewRat(1000, 1), NetAssetValue: big.NewRat(900, 1), VaR: big.NewRat(5, 1), GrossExposure: big.NewRat(1200, 1)}, at, filing.HashSigner{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Report(report, id.String(), &domainpb.MeasureProvenance{Method: string(riskv1.MethodPortfolioArithmetic), InputManifestDigest: input})
	if err != nil {
		t.Fatal(err)
	}
	body := &regulatorypb.FilingRecordBody{Report: wire, TenantId: "acme", PortfolioId: "pf1", InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: input, ProducerVersion: "synthetic-test", FilingCanonical: report.Canonical(), SignatureKind: regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH, RecordedAt: timestamppb.New(at.Add(24 * time.Hour))}
	canonical, err := BindingCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	record, err := Recorded(body, signer.Link{Prev: chain.Genesis, Cur: chain.Next(chain.Genesis, canonical), Body: canonical})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPublishRegulatorySignedArtifactReachesTheBroker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server, err := ns.NewServer(&ns.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(), MaxPayload: 1 << 20, MaxConn: 8, JetStreamMaxMemory: 4 << 20, JetStreamMaxStore: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	defer func() { server.Shutdown(); server.WaitForShutdown() }()
	if !server.ReadyForConnections(3 * time.Second) {
		t.Fatal("owned loopback broker did not become ready")
	}
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "REGULATORY_PUBLISH", Subjects: []string{RecordedSubject}, Storage: jetstream.MemoryStorage, MaxBytes: 2 << 20, MaxMsgs: 16, Duplicates: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: server.ClientURL(), Name: "regulatory-publish-test", DisableReconnect: true, PublishTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Error("publish client cleanup failed")
		}
	}()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-test", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := New(producer, "acme")
	if err != nil {
		t.Fatal(err)
	}
	record := syntheticRecord(t)
	event, err := Event(record)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := outbox.From(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := stored.Event()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.EventID != "" {
		t.Fatal("test changed canonical Record's producer-owned envelope identity contract")
	}
	for range 2 {
		if err := publisher.Publish(ctx, replayed); err != nil {
			t.Fatal(err)
		}
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("persisted domain identity did not deduplicate: %d", info.State.Msgs)
	}
	message, err := stream.GetMsg(ctx, info.State.LastSeq)
	if err != nil {
		t.Fatal("recorded filing FACT missing from actual broker")
	}
	envelope, payload, err := bus.Unframe(message.Data)
	if err != nil || bus.Validate(envelope) != nil {
		t.Fatal("actual regulatory envelope invalid")
	}
	var observed regulatorypb.FilingRecorded
	if proto.Unmarshal(payload, &observed) != nil || !proto.Equal(&observed, record) || envelope.EventId != record.Body.Report.ReportId || envelope.IdempotencyKey != envelope.EventId || envelope.TenantId != "acme" || !envelope.EventTime.AsTime().Equal(record.Body.RecordedAt.AsTime()) {
		t.Fatal("actual signed artifact/scope/identity changed in transit")
	}
	rebuilt, err := FromWire(observed.Body.Report)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := (filing.HashSigner{}).Sign(rebuilt.Canonical())
	if err != nil || signature != observed.Body.Report.Signature || observed.Body.Report.LineItems[0].Provenance.InputManifestDigest != observed.Body.InputManifestDigest {
		t.Fatal("actual report cannot reproduce its original signature/provenance")
	}
	for _, mismatch := range []bool{false, true} {
		record := syntheticFRTB(t, mismatch)
		event, err := Event(record)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := outbox.From(ctx, event)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := stored.Event()
		if err != nil {
			t.Fatal(err)
		}
		if err := publisher.Publish(ctx, replay); err != nil {
			t.Fatal(err)
		}
		info, err := stream.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		message, err := stream.GetMsg(ctx, info.State.LastSeq)
		if err != nil {
			t.Fatal("FRTB actual broker publication missing")
		}
		_, raw, err := bus.Unframe(message.Data)
		if err != nil {
			t.Fatal(err)
		}
		var observed regulatorypb.FilingRecorded
		if proto.Unmarshal(raw, &observed) != nil || !proto.Equal(&observed, record) {
			t.Fatal("FRTB filed rows changed in transit")
		}
		charge := observed.Body.Report.CapitalCharge
		wantTotal := "15"
		if mismatch {
			wantTotal = "15.00000001"
		}
		want, ok := dec.ParseProtoExact(wantTotal)
		if !ok || len(observed.Body.Report.LineItems) != 6 || charge == nil || !proto.Equal(charge.Total, want) || (!mismatch && len(charge.Components) != 5) || (mismatch && len(charge.Components) != 0) {
			t.Fatal("FRTB capital projection invented a residual or lost filed rows")
		}
		for _, line := range observed.Body.Report.LineItems {
			if line.Code != "FRTB_TOTAL" {
				value, ok := dec.FromProtoChecked(line.Value)
				if !ok || !value.IsInt() {
					t.Fatal("FRTB independently rounded row changed")
				}
			}
		}
	}
}

func syntheticFRTB(t *testing.T, mismatch bool) *regulatorypb.FilingRecorded {
	t.Helper()
	values := map[string]*big.Rat{}
	for i, code := range []string{"FRTB_DELTA", "FRTB_VEGA", "FRTB_CURVATURE", "FRTB_DRC", "FRTB_RRAO"} {
		value := new(big.Rat).SetInt64(int64(i + 1))
		if mismatch {
			value.Add(value, big.NewRat(1, 1000000000))
		}
		values[code] = value
	}
	values["FRTB_TOTAL"] = big.NewRat(15, 1)
	if mismatch {
		values["FRTB_TOTAL"].Add(values["FRTB_TOTAL"], big.NewRat(5, 1000000000))
	}
	manifest, err := json.Marshal(struct {
		Origin      string
		FiledInputs map[string]*big.Rat
	}{"filing-request/unverified", values})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	input := hex.EncodeToString(digest[:])
	at := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	report, err := regulatory.BuildReport(regulatory.FRTB, at, values, filing.HashSigner{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Report(report, id.String(), &domainpb.MeasureProvenance{Method: string(riskv1.MethodFRTBStandardised), InputManifestDigest: input})
	if err != nil {
		t.Fatal(err)
	}
	body := &regulatorypb.FilingRecordBody{Report: wire, TenantId: "acme", PortfolioId: "pf1", InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: manifest, InputManifestDigest: input, ProducerVersion: "literal-filed-row-oracle", FilingCanonical: report.Canonical(), SignatureKind: regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH, RecordedAt: timestamppb.New(at.Add(time.Hour))}
	canonical, err := BindingCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	record, err := Recorded(body, signer.Link{Prev: chain.Genesis, Cur: chain.Next(chain.Genesis, canonical), Body: canonical})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPublishContextWitnessBindsFullPreimageWithoutStoringItGlobally(t *testing.T) {
	record := syntheticRecord(t)
	full, err := (proto.MarshalOptions{Deterministic: true}).Marshal(record.Body)
	if err != nil {
		t.Fatal(err)
	}
	witness, err := BindingCanonical(record.Body)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(full)
	if len(witness) != len(bindingDomain)+8+sha256.Size || string(witness[:len(bindingDomain)]) != bindingDomain || binary.BigEndian.Uint64(witness[len(bindingDomain):len(bindingDomain)+8]) != uint64(len(full)) || !bytes.Equal(witness[len(bindingDomain)+8:], digest[:]) {
		t.Fatal("context witness does not bind the complete retained preimage")
	}
	if bytes.Contains(witness, full) {
		t.Fatal("global context witness stored the private full preimage")
	}
}

func TestPublishRegulatoryRejectsAlteredContextAndSignature(t *testing.T) {
	for _, mutation := range []string{"tenant", "portfolio", "input", "signature", "canonical", "context-hash", "provenance", "recorded-time", "capital"} {
		t.Run(mutation, func(t *testing.T) {
			record := syntheticRecord(t)
			switch mutation {
			case "tenant":
				record.Body.TenantId = "foreign"
			case "portfolio":
				record.Body.PortfolioId = "foreign"
			case "input":
				record.Body.InputManifest = append(record.Body.InputManifest, ' ')
			case "signature":
				record.Body.Report.Signature = record.BindingHash
			case "canonical":
				record.Body.FilingCanonical = append(record.Body.FilingCanonical, ' ')
			case "context-hash":
				record.BindingHash = record.Body.Report.Signature
			case "provenance":
				record.Body.Report.LineItems[0].Provenance = nil
			case "recorded-time":
				record.Body.RecordedAt = nil
			case "capital":
				record.Body.Report.CapitalCharge = &regulatorypb.CapitalCharge{}
			}
			if _, err := Event(record); err == nil {
				t.Fatal("altered context/signature was publishable")
			}
		})
	}
}
