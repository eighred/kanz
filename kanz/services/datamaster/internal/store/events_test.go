package store

// THE OVERRIDE FACT (#410).
//
// #410's acceptance: "the approval is recorded as a FACT carrying BOTH
// identities". These tests are about the ways a FACT can exist and still not say
// that.

import (
	"math/big"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	masterpb "github.com/eighred/kanz/kanz-schemas-go/master/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

var evNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func exceptionFixture() pricing.Exception {
	return pricing.Exception{
		ID: "INST1:PRICE_TOLERANCE:ICE", InstrumentID: "INST1",
		Kind: pricing.KindPriceTolerance, Status: pricing.StatusOverridden,
	}
}

func dualSigned() pricing.Override {
	return pricing.Override{
		Actor: "alice@kanz", Approver: "bob@kanz", Reason: "vendor confirmed",
		ChosenPrice: dec.Rat("130.25"), At: evNow,
	}
}

// BOTH IDENTITIES REACH THE WIRE.
func TestOverrideEvent_CarriesBothIdentities(t *testing.T) {
	ev, err := overrideEvent(exceptionFixture(), dualSigned())
	if err != nil {
		t.Fatalf("overrideEvent: %v", err)
	}
	payload, ok := ev.Payload.(*masterpb.ExceptionOverridden)
	if !ok {
		t.Fatalf("payload is %T", ev.Payload)
	}
	if got := payload.GetOverride().GetActor(); got != "alice@kanz" {
		t.Errorf("actor = %q, want the proposer", got)
	}
	if got := payload.GetOverride().GetApprover(); got != "bob@kanz" {
		t.Errorf("approver = %q, want bob@kanz — a FACT that names only one person is "+
			"indistinguishable from the single-signature world it replaced", got)
	}
	if payload.GetExceptionId() != "INST1:PRICE_TOLERANCE:ICE" || payload.GetInstrumentId() != "INST1" {
		t.Errorf("payload does not identify the exception: %+v", payload)
	}
	if payload.GetStatus() != masterpb.ExceptionStatus_EXCEPTION_STATUS_OVERRIDDEN {
		t.Errorf("status = %v, want OVERRIDDEN", payload.GetStatus())
	}
}

// A SINGLE-SIGNED OVERRIDE IS ANNOUNCED AS SUCH, not omitted.
//
// Dual control ships unarmed, so most overrides today are one person's decision.
// If those produced no FACT, the audit trail would contain exactly the overrides
// that needed the least scrutiny and none of the ones that needed the most.
func TestOverrideEvent_ASingleSignedOverrideIsStillAnnounced(t *testing.T) {
	o := dualSigned()
	o.Approver = ""
	ev, err := overrideEvent(exceptionFixture(), o)
	if err != nil {
		t.Fatalf("a single-signed override produced no FACT: %v", err)
	}
	payload := ev.Payload.(*masterpb.ExceptionOverridden)
	if payload.GetOverride().GetApprover() != "" {
		t.Errorf("approver = %q, want empty", payload.GetOverride().GetApprover())
	}
	if payload.GetOverride().GetActor() != "alice@kanz" {
		t.Error("the actor did not survive")
	}
}

// THE ENVELOPE IS A FACT, ON THE SUBJECT A STREAM CARRIES.
//
// A COMMAND-classed event would be routed and retried as an instruction; a
// subject no stream carries is a HARD publish error that would fail the override
// transaction. Both are silent until a real broker sees them.
func TestOverrideEvent_EnvelopeShape(t *testing.T) {
	ev, err := overrideEvent(exceptionFixture(), dualSigned())
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", ev.EventClass)
	}
	if ev.Subject != "data.exception.overridden" {
		t.Errorf("subject = %q", ev.Subject)
	}
	if !strings.HasPrefix(ev.Subject, "data.") {
		t.Errorf("subject %q is outside the DATA stream's data.> pattern, so the publish is a hard "+
			"error and the override transaction fails", ev.Subject)
	}
	// PARTITIONED BY EXCEPTION, so one exception's overrides stay ordered.
	if ev.PartitionKey != "INST1:PRICE_TOLERANCE:ICE" {
		t.Errorf("partition key = %q, want the exception id", ev.PartitionKey)
	}
	if ev.EventTime != evNow {
		t.Errorf("event time = %v, want when the override HAPPENED", ev.EventTime)
	}
	if !strings.Contains(ev.PayloadSchemaRef, "ExceptionOverridden") {
		t.Errorf("payload schema ref = %q", ev.PayloadSchemaRef)
	}
}

// THE PRICE IS EXACT ON THE WIRE, or there is no FACT.
//
// The override price is a named human's decision. A figure with more precision
// than the platform's fixed scale is REFUSED rather than rounded — announcing a
// price nobody chose is worse than announcing nothing, and it would be the
// version an auditor reads.
func TestOverrideEvent_TheChosenPriceIsExactOrRefused(t *testing.T) {
	o := dualSigned()
	ev, err := overrideEvent(exceptionFixture(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := dec.FromProto(ev.Payload.(*masterpb.ExceptionOverridden).GetOverride().GetChosenPrice())
	if got.Cmp(dec.Rat("130.25")) != 0 {
		t.Errorf("price on the wire = %s, want 130.25 exactly", got.RatString())
	}

	// More precision than the platform's scale: refused, not rounded.
	tooPrecise, ok := new(big.Rat).SetString("130.1234567890123456789")
	if !ok {
		t.Fatal("bad fixture")
	}
	o.ChosenPrice = tooPrecise
	if _, err := overrideEvent(exceptionFixture(), o); err == nil {
		t.Error("an over-precise price was announced — the FACT would carry a number nobody chose")
	}
}

// A ZERO TIMESTAMP IS REFUSED.
//
// On the wire it renders as the Unix epoch and reads as a decision made in 1970;
// stamping now() instead would assert it happened at publish time. Both are
// false, so neither is written.
func TestOverrideEvent_AZeroTimestampIsRefused(t *testing.T) {
	o := dualSigned()
	o.At = time.Time{}
	if _, err := overrideEvent(exceptionFixture(), o); err == nil {
		t.Fatal("an override with no timestamp produced a FACT")
	}
}

// AN UNKNOWN STATUS IS UNSPECIFIED, NOT A GUESS.
func TestOverrideEvent_AnUnknownStatusIsNotAsserted(t *testing.T) {
	ex := exceptionFixture()
	ex.Status = pricing.Status("SOMETHING_NEW")
	ev, err := overrideEvent(ex, dualSigned())
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.Payload.(*masterpb.ExceptionOverridden).GetStatus(); got != masterpb.ExceptionStatus_EXCEPTION_STATUS_UNSPECIFIED {
		t.Errorf("status = %v for an unknown status, want UNSPECIFIED", got)
	}
}

// THE EVENT SURVIVES THE OUTBOX ROUND TRIP.
//
// The record stores the payload as bytes plus a schema ref, and the relay
// rebuilds the event from those alone. If the pair does not round-trip, the FACT
// is enqueued and then fails to publish forever — a backlog that climbs with no
// bad input to point at.
func TestOverrideEvent_SurvivesTheOutboxRoundTrip(t *testing.T) {
	ev, err := overrideEvent(exceptionFixture(), dualSigned())
	if err != nil {
		t.Fatal(err)
	}
	// WITHOUT A TENANT THE RECORD IS REFUSED, and this is asserted first because
	// it is the failure that would have shipped. bus/outbox resolve a tenant from
	// the INBOUND DELIVERY a handler runs in; an override arrives over HTTP, so
	// there is no delivery and no tenant on the context. PostgresExceptions sets
	// it explicitly from the tenant it was constructed with — and if that ever
	// stops happening, EVERY override on the durable path fails inside the
	// transaction, because the enqueue is part of it.
	if _, err := outbox.From(t.Context(), ev); err == nil {
		t.Fatal("a FACT with no tenant was accepted by the outbox — the check that forces " +
			"PostgresExceptions to set one has gone")
	}
	ev.TenantID = "__system__" // what PostgresExceptions.Override does

	rec, err := outbox.From(t.Context(), ev)
	if err != nil {
		t.Fatalf("outbox.From: %v", err)
	}
	back, err := rec.Event()
	if err != nil {
		t.Fatalf("Record.Event: %v — the relay could never publish this record", err)
	}
	payload, ok := back.Payload.(*masterpb.ExceptionOverridden)
	if !ok {
		t.Fatalf("payload came back as %T", back.Payload)
	}
	if payload.GetOverride().GetApprover() != "bob@kanz" || payload.GetOverride().GetActor() != "alice@kanz" {
		t.Errorf("an identity was lost in the outbox round trip: actor=%q approver=%q",
			payload.GetOverride().GetActor(), payload.GetOverride().GetApprover())
	}
	if back.Subject != ev.Subject || back.EventClass != ev.EventClass {
		t.Errorf("envelope changed: %q/%v -> %q/%v", ev.Subject, ev.EventClass, back.Subject, back.EventClass)
	}
	if got := dec.FromProto(payload.GetOverride().GetChosenPrice()); got.Cmp(dec.Rat("130.25")) != 0 {
		t.Errorf("price after round trip = %s, want 130.25", got.RatString())
	}
}
