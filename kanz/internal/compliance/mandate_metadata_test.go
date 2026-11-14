package compliance

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func missingMetadata(field string) *compliancepb.Mandate {
	m := identityMandate("a", "shared", 3)
	switch field {
	case "mandate_id":
		m.MandateId = ""
	case "version":
		m.Version = 0
	case "effective_at":
		m.EffectiveAt = nil
	}
	return m
}

func TestMandateMetadataLoaderPreflightIsAtomic(t *testing.T) {
	for _, field := range []string{"mandate_id", "version", "effective_at"} {
		t.Run(field, func(t *testing.T) {
			for _, shape := range []string{"legacy", "bad_first", "bad_last"} {
				t.Run(shape, func(t *testing.T) {
					for _, state := range []string{"cold", "warm", "rejected"} {
						t.Run(state, func(t *testing.T) {
							reg := NewMandateRegistry()
							mustPut(t, reg, identityMandate("b", "shared", 1))
							mustPut(t, reg, identityMandate("a", "other", 1))
							if state != "cold" {
								mustPut(t, reg, identityMandate("a", "shared", 1))
							}
							key := mandateKey{"a", "shared"}
							cause := errors.New("previous rejection")
							if state == "rejected" {
								reg.Reject("a", "shared", cause)
							}
							before := make(map[mandateKey][]*compliancepb.Mandate)
							for k, versions := range reg.byKey {
								for _, m := range versions {
									before[k] = append(before[k], proto.Clone(m).(*compliancepb.Mandate))
								}
							}
							bad := missingMetadata(field)
							value, err := MarshalMandateValue(bad)
							if err != nil {
								t.Fatal(err)
							}
							if shape != "legacy" {
								members := []*compliancepb.Mandate{bad, identityMandate("a", "shared", 2)}
								if shape == "bad_last" {
									members[0], members[1] = members[1], members[0]
								}
								value = identityValue(t, members...)
							}
							_, err = NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{ConfigKey: MandateConfigKey("a", "shared"), NewValue: value})
							if !errors.Is(err, ErrMandateIncomplete) {
								t.Fatalf("required %s accepted: %v", field, err)
							}
							if len(before) != len(reg.byKey) {
								t.Fatal("failed preflight created a registry key")
							}
							for k, versions := range before {
								got := reg.byKey[k]
								if len(got) != len(versions) {
									t.Fatalf("failed preflight changed %v", k)
								}
								for i, m := range versions {
									if !proto.Equal(got[i], m) {
										t.Fatalf("failed preflight replaced %v", k)
									}
								}
							}
							if state == "rejected" && reg.rejected[key] != cause || state != "rejected" && reg.rejected[key] != nil {
								t.Fatal("failed preflight changed rejection marker")
							}
						})
					}
				})
			}
		})
	}
}

func TestMandateMetadataConsumerRefusesAndRepairs(t *testing.T) {
	for _, field := range []string{"mandate_id", "version", "effective_at"} {
		t.Run(field, func(t *testing.T) {
			for _, state := range []string{"cold", "warm"} {
				t.Run(state, func(t *testing.T) {
					reg := NewMandateRegistry()
					victim := identityMandate("b", "shared", 1)
					other := identityMandate("a", "other", 1)
					mustPut(t, reg, victim)
					mustPut(t, reg, other)
					if state == "warm" {
						mustPut(t, reg, identityMandate("a", "shared", 1))
					}
					reg.Arm()
					rejected := 0
					consumer := NewMandateConsumer(reg, nil, WithMandateRejectionObserver(func(tenant, portfolio string) {
						if tenant != "a" || portfolio != "shared" {
							t.Error("rejection attributed to another key")
						}
						rejected++
					}))
					env := &envelopepb.Envelope{TenantId: "a"}
					key := MandateConfigKey("a", "shared")
					bad := configChanged(t, key, identityValue(t, identityMandate("a", "shared", 2), missingMetadata(field)))
					for range 2 {
						if err := consumer.Handle(context.Background(), env, bad); err != nil {
							t.Fatalf("invalid metadata must retain ACK contract: %v", err)
						}
						if _, _, err := reg.Mandate(context.Background(), "a", "shared", time.Now()); !errors.Is(err, ErrMandateUnreadable) {
							t.Fatalf("metadata poison read as governed: %v", err)
						}
						for _, required := range []bool{false, true} {
							gate := NewPreTradeGate(nil, nil, reg, nil, nil, nil, WithRequireMandate(required))
							d, err := gate.Evaluate(context.Background(), OrderDelta{TenantID: "a", PortfolioID: "shared", InstrumentID: "A", SignedQuantity: dec(1, 0), Price: dec(10, 0), Currency: "USD", AsOf: time.Now()})
							if err != nil || d.Allowed || !d.Unreadable || d.Ungoverned {
								t.Fatalf("unknown metadata bypassed mandate posture %v: %+v %v", required, d, err)
							}
						}
						if !reg.Armed() || reg.Complete() {
							t.Fatal("ACK/armed registry claimed complete governance")
						}
					}
					if rejected != 2 {
						t.Fatalf("refusal observer calls=%d", rejected)
					}
					current := identityMandate("a", "shared", 10)
					future := identityMandate("a", "shared", 20)
					future.EffectiveAt = timestamppb.New(time.Now().UTC().Add(24 * time.Hour))
					for range 2 {
						if err := consumer.Handle(context.Background(), env, configChanged(t, key, identityValue(t, current, future))); err != nil {
							t.Fatal(err)
						}
					}
					for _, m := range []*compliancepb.Mandate{current, future, victim, other} {
						at := time.Now()
						if m == future {
							at = future.EffectiveAt.AsTime()
						}
						got, gov, err := reg.Mandate(context.Background(), m.TenantId, m.PortfolioId, at)
						if err != nil || gov != Governed || !proto.Equal(got, m) {
							t.Fatalf("repair/schedule/isolation failed: %v %v", got, err)
						}
					}
					if !reg.Complete() || len(reg.Rejections()) != 0 || len(reg.byKey[mandateKey{"a", "shared"}]) != 2 {
						t.Fatal("valid repeat repair did not restore complete bounded schedule")
					}
				})
			}
		})
	}
}

func TestMandateMetadataLegalBoundariesStillGovern(t *testing.T) {
	for _, name := range []string{"epoch", "year_one", "max_version"} {
		t.Run(name, func(t *testing.T) {
			m := identityMandate("a", "shared", 1)
			if name == "year_one" {
				m.EffectiveAt = timestamppb.New(time.Time{})
			}
			if name == "max_version" {
				m.Version = math.MaxUint64
			}
			value, err := MarshalMandateValue(m)
			if err != nil {
				t.Fatal(err)
			}
			reg := NewMandateRegistry()
			if _, err := NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{ConfigKey: MandateConfigKey("a", "shared"), NewValue: value}); err != nil {
				t.Fatal(err)
			}
			gate := NewPreTradeGate(nil, nil, reg, nil, nil, nil, WithRequireMandate(true))
			d, err := gate.Evaluate(context.Background(), OrderDelta{TenantID: "a", PortfolioID: "shared", InstrumentID: "A", SignedQuantity: dec(1, 0), Price: dec(10, 0), Currency: "USD", AsOf: time.Now()})
			if err != nil || !d.Allowed || !d.Unconstrained {
				t.Fatalf("legal zero-rule mandate refused: %+v %v", d, err)
			}
			got, gov, err := reg.Mandate(context.Background(), "a", "shared", time.Now())
			if err != nil || gov != Governed || !proto.Equal(got, m) {
				t.Fatalf("legal metadata changed: %v %v", got, err)
			}
		})
	}
}
