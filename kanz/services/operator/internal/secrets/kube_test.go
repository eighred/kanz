package secrets

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubeStoreSetVenueKeysAppliesSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewKubeStore(cs, "kanz-services")
	if err := s.SetVenueKeys(context.Background(), "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}); err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	sec, err := cs.CoreV1().Secrets("kanz-services").Get(context.Background(), "venue-okx-keys", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %v, want Opaque", sec.Type)
	}
	for k, want := range map[string]string{"api_key": "k", "api_secret": "s", "api_passphrase": "p"} {
		if got := string(sec.Data[k]); got != want {
			t.Errorf("data[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestKubeStoreSetVenueKeysUpdatesInPlace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewKubeStore(cs, "kanz-services")
	ctx := context.Background()
	_ = s.SetVenueKeys(ctx, "binance", VenueKeys{APIKey: "k1", APISecret: "s1"})
	if err := s.SetVenueKeys(ctx, "binance", VenueKeys{APIKey: "k2", APISecret: "s2"}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	sec, _ := cs.CoreV1().Secrets("kanz-services").Get(ctx, "venue-binance-keys", metav1.GetOptions{})
	if got := string(sec.Data["api_key"]); got != "k2" {
		t.Errorf("api_key = %q, want k2 (updated in place)", got)
	}
	if _, ok := sec.Data["api_passphrase"]; ok {
		t.Errorf("binance secret must carry no api_passphrase")
	}
}

func TestKubeStoreListVenues(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewKubeStore(cs, "kanz-services")
	ctx := context.Background()
	_ = s.SetVenueKeys(ctx, "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"})
	got, err := s.ListVenues(ctx)
	if err != nil {
		t.Fatalf("ListVenues: %v", err)
	}
	byVenue := map[string]bool{}
	for _, v := range got {
		byVenue[v.Venue] = v.Configured
	}
	if !byVenue["okx"] {
		t.Errorf("okx should be configured")
	}
	if byVenue["binance"] {
		t.Errorf("binance should be unconfigured")
	}
	if _, ok := byVenue["binance"]; !ok {
		t.Errorf("ListVenues must report ALL known venues, including unconfigured ones")
	}
	_ = apierrors.IsNotFound // referenced by kube.go
}
