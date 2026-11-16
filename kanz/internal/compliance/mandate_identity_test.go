package compliance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func identityMandate(tenant, portfolio string, version uint64) *compliancepb.Mandate {
	return &compliancepb.Mandate{MandateId: "m-" + tenant + "-" + portfolio, TenantId: tenant, PortfolioId: portfolio, Version: version, EffectiveAt: timestamppb.New(time.Unix(0, 0))}
}

func identityValue(t *testing.T, members ...*compliancepb.Mandate) string {
	t.Helper()
	v, err := MarshalMandateSetValue(members)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMandateIdentityLoaderPreflightIsAtomic(t *testing.T) {
	for _, field := range []string{"foreign-tenant", "foreign-portfolio", "missing-tenant", "missing-portfolio"} {
		for _, first := range []bool{false, true} {
			for _, rejected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/first=%v/rejected=%v", field, first, rejected), func(t *testing.T) {
					reg := NewMandateRegistry()
					original := identityMandate("a", "shared", 1)
					victim := identityMandate("b", "shared", 1)
					mustPut(t, reg, original)
					mustPut(t, reg, victim)
					mustPut(t, reg, identityMandate("a", "other", 1))
					if rejected {
						reg.Reject("a", "shared", errors.New("previous refusal"))
					}
					good := identityMandate("a", "shared", 2)
					bad := identityMandate("a", "shared", 3)
					switch field {
					case "foreign-tenant":
						bad.TenantId = "b"
					case "foreign-portfolio":
						bad.PortfolioId = "other"
					case "missing-tenant":
						bad.TenantId = ""
					case "missing-portfolio":
						bad.PortfolioId = ""
					}
					members := []*compliancepb.Mandate{good, bad}
					if first {
						members[0], members[1] = members[1], members[0]
					}
					if _, err := NewMandateLoader(reg).Apply(&lifecyclepb.ConfigChanged{ConfigKey: MandateConfigKey("a", "shared"), NewValue: identityValue(t, members...)}); err == nil {
						t.Fatal("mixed identity set accepted")
					}
					// Inspect the stored versions as well as the public refusal: marking
					// the key unreadable cannot hide a partial replacement beneath it.
					for _, key := range []mandateKey{{"a", "shared"}, {"b", "shared"}, {"a", "other"}} {
						if versions := reg.byKey[key]; len(versions) != 1 || versions[0].GetVersion() != 1 {
							t.Fatalf("partially mutated %v: %v", key, versions)
						}
					}
					if (reg.rejected[mandateKey{"a", "shared"}] != nil) != rejected {
						t.Fatal("failed preflight cleared an earlier rejection")
					}
				})
			}
		}
	}
}

func TestMandateIdentityConsumerScopesRejections(t *testing.T) {
	for _, form := range []string{"trusted-poison", "foreign-envelope", "missing-envelope", "missing-envelope-tenant", "malformed-key"} {
		t.Run(form, func(t *testing.T) {
			reg := NewMandateRegistry()
			victim := identityMandate("b", "shared", 1)
			mustPut(t, reg, victim)
			var rejected []string
			consumer := NewMandateConsumer(reg, nil, WithMandateRejectionObserver(func(tenant, portfolio string) { rejected = append(rejected, tenant+"/"+portfolio) }))
			e := &envelopepb.Envelope{TenantId: "a"}
			key := MandateConfigKey("a", "shared")
			switch form {
			case "foreign-envelope":
				key = MandateConfigKey("b", "shared")
			case "missing-envelope":
				e = nil
			case "missing-envelope-tenant":
				e = &envelopepb.Envelope{}
			case "malformed-key":
				key = MandateConfigKeyPrefix + "broken"
			}
			if err := consumer.Handle(context.Background(), e, configChanged(t, key, identityValue(t, identityMandate("b", "shared", 9)))); err != nil {
				t.Fatalf("poison must retain existing ACK contract: %v", err)
			}
			got, gov, err := reg.Mandate(context.Background(), "b", "shared", time.Now())
			if err != nil || gov != Governed || !proto.Equal(got, victim) {
				t.Fatalf("foreign victim was replaced or rejected: mandate=%v governance=%v error=%v", got, gov, err)
			}
			if form == "trusted-poison" {
				if len(rejected) != 1 || rejected[0] != "a/shared" {
					t.Fatalf("trusted key refusal attributed to %v", rejected)
				}
				if _, _, err := reg.Mandate(context.Background(), "a", "shared", time.Now()); !errors.Is(err, ErrMandateUnreadable) {
					t.Fatalf("trusted poisoned key not unreadable: %v", err)
				}
			} else if len(rejected) != 0 || len(reg.Rejections()) != 0 || reg.Dropped() != 1 {
				t.Fatal("unattributable identity must be counted without rejecting a victim")
			}
		})
	}
}

func TestMandateIdentityPreservesLegacySchedulesAndScopedRepair(t *testing.T) {
	reg := NewMandateRegistry()
	consumer := NewMandateConsumer(reg, nil)
	env := &envelopepb.Envelope{TenantId: "a"}
	key := MandateConfigKey("a", "shared")
	first := identityMandate("a", "shared", 1)
	legacy, err := MarshalMandateValue(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(context.Background(), env, configChanged(t, key, legacy)); err != nil {
		t.Fatal(err)
	}
	if got, _, err := reg.Mandate(context.Background(), "a", "shared", time.Now()); err != nil || !proto.Equal(got, first) {
		t.Fatal("legacy single mandate did not govern")
	}
	if err := consumer.Handle(context.Background(), env, configChanged(t, key, identityValue(t, first, identityMandate("b", "shared", 2)))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Mandate(context.Background(), "a", "shared", time.Now()); !errors.Is(err, ErrMandateUnreadable) {
		t.Fatal("poison did not invalidate trusted key")
	}
	future := identityMandate("a", "shared", 2)
	future.EffectiveAt = timestamppb.New(time.Now().UTC().Add(24 * time.Hour))
	value := identityValue(t, first, future)
	for range 2 {
		if err := consumer.Handle(context.Background(), env, configChanged(t, key, value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{time.Now(), future.EffectiveAt.AsTime()} {
		want := first
		if at.Equal(future.EffectiveAt.AsTime()) {
			want = future
		}
		got, gov, err := reg.Mandate(context.Background(), "a", "shared", at)
		if err != nil || gov != Governed || !proto.Equal(got, want) {
			t.Fatalf("schedule/repair changed at %v: %v %v", at, got, err)
		}
	}
	if !reg.Complete() || len(reg.byKey[mandateKey{"a", "shared"}]) != 2 {
		t.Fatal("valid duplicate repair lost complete scheduled set")
	}
}
