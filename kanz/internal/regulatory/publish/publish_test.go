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
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
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
		// Literal economic/filed oracles: do not obtain expected values from
		// Report or FromWire, which are both part of the producer under test.
		wantRows := map[string]string{"FRTB_DELTA": "1", "FRTB_VEGA": "2", "FRTB_CURVATURE": "3", "FRTB_DRC": "4", "FRTB_RRAO": "5", "FRTB_TOTAL": wantTotal}
		want, valid := new(big.Rat).SetString(wantTotal)
		if !valid || len(observed.Body.Report.LineItems) != len(wantRows) || charge == nil || charge.Framework != regulatorypb.Framework_FRAMEWORK_FRTB || (!mismatch && len(charge.Components) != 5) || (mismatch && len(charge.Components) != 0) {
			t.Fatal("FRTB capital projection invented a residual or lost filed rows")
		}
		actualTotal, valid := dec.FromProtoChecked(charge.Total)
		if !valid || charge.Total == nil || actualTotal.Cmp(want) != 0 {
			t.Fatal("FRTB capital total differs from its literal filed value")
		}
		seenRows := make(map[string]bool)
		for _, line := range observed.Body.Report.LineItems {
			if line == nil {
				t.Fatal("FRTB literal row missing")
			}
			text, present := wantRows[line.Code]
			expected, ok := new(big.Rat).SetString(text)
			value, valid := dec.FromProtoChecked(line.Value)
			if !present || seenRows[line.Code] || !ok || !valid || line.Value == nil || value.Cmp(expected) != 0 {
				t.Fatal("FRTB code-to-filed-value mapping differs from literal oracle")
			}
			seenRows[line.Code] = true
		}
		seenComponents := make(map[string]bool)
		componentSum := new(big.Rat)
		for _, component := range charge.Components {
			if component == nil || component.Name == "FRTB_TOTAL" {
				t.Fatal("FRTB capital component name invalid")
			}
			text, present := wantRows[component.Name]
			expected, ok := new(big.Rat).SetString(text)
			amount, valid := dec.FromProtoChecked(component.Amount)
			if !present || seenComponents[component.Name] || !ok || !valid || component.Amount == nil || amount.Cmp(expected) != 0 {
				t.Fatal("FRTB component-name-to-amount mapping differs from literal oracle")
			}
			seenComponents[component.Name] = true
			componentSum.Add(componentSum, amount)
		}
		if !mismatch && componentSum.Cmp(big.NewRat(15, 1)) != 0 {
			t.Fatal("FRTB literal component sum changed")
		}
		canonical := []byte("FRTB\n2026-06-30T00:00:00Z\nFRTB_CURVATURE=3\nFRTB_DELTA=1\nFRTB_DRC=4\nFRTB_RRAO=5\nFRTB_TOTAL=" + wantTotal + "\nFRTB_VEGA=2\n")
		signature := sha256.Sum256(canonical)
		if !bytes.Equal(observed.Body.FilingCanonical, canonical) || observed.Body.Report.Signature != hex.EncodeToString(signature[:]) {
			t.Fatal("FRTB signed canonical artifact differs from literal filed rows")
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
	t.Run("surplus-sub-filing-scale-digits", func(t *testing.T) {
		record := syntheticRecord(t)
		original, err := FromWire(record.Body.Report)
		if err != nil {
			t.Fatal(err)
		}
		changed := 0
		for i, line := range record.Body.Report.LineItems {
			if line.Code == "FORM_PF_GROSS_NAV" {
				line.Value = &commonpb.Decimal{Coefficient: 1000000000001, Exponent: -9}
				original.LineItems[i].Value = big.NewRat(1000000000001, 1000000000)
				changed++
			}
		}
		if changed != 1 || !bytes.Equal(original.Canonical(), record.Body.FilingCanonical) {
			t.Fatal("surplus-digit control did not preserve the original rendered canonical artifact")
		}
		if _, err := FromWire(record.Body.Report); err == nil {
			t.Fatal("wire retained digits that the original filed signature does not describe")
		}
		// Build a mathematically consistent witness independently of the typed
		// validator. Refusal must come from the filed-value invariant, not merely
		// from leaving an obsolete context hash on an otherwise changed body.
		full, err := (proto.MarshalOptions{Deterministic: true}).Marshal(record.Body)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(full)
		witness := make([]byte, len(bindingDomain)+8+sha256.Size)
		copy(witness, bindingDomain)
		binary.BigEndian.PutUint64(witness[len(bindingDomain):], uint64(len(full)))
		copy(witness[len(bindingDomain)+8:], digest[:])
		link := signer.Link{Prev: chain.Genesis, Cur: chain.Next(chain.Genesis, witness), Body: witness}
		if _, err := Recorded(record.Body, link); err == nil {
			t.Fatal("consistent context witness admitted surplus unfiled digits")
		}
		record.BindingPrevHash, record.BindingHash = link.Prev, link.Cur
		if _, err := Event(record); err == nil {
			t.Fatal("event admitted surplus unfiled digits despite a consistent witness")
		}
	})
	t.Run("equivalent-decimal-encoding", func(t *testing.T) {
		record := syntheticRecord(t)
		originalSignature := record.Body.Report.Signature
		changed := 0
		for _, line := range record.Body.Report.LineItems {
			if line.Code == "FORM_PF_GROSS_NAV" {
				equivalent := &commonpb.Decimal{Coefficient: 10000, Exponent: -1}
				if proto.Equal(line.Value, equivalent) {
					t.Fatal("equivalent-encoding control did not change coefficient/exponent representation")
				}
				line.Value = equivalent
				changed++
			}
		}
		decoded, err := FromWire(record.Body.Report)
		if changed != 1 || err != nil || !bytes.Equal(decoded.Canonical(), record.Body.FilingCanonical) {
			t.Fatal("equivalent exact filed value was refused or changed its canonical rendering")
		}
		witness, err := BindingCanonical(record.Body)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := Recorded(record.Body, signer.Link{Prev: chain.Genesis, Cur: chain.Next(chain.Genesis, witness), Body: witness})
		if err != nil || bound.Body.Report.Signature != originalSignature {
			t.Fatal("equivalent encoding changed the original signature or could not be recorded")
		}
		if _, err := Event(bound); err != nil {
			t.Fatal("equivalent exact encoding could not be published")
		}
	})
}
