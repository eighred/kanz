package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// fieldManager is the name stamped into managedFields on every Create/Update of
// a venue-key Secret. There is no server-side apply verb here — it is plain
// Create/Update — so this is purely an audit tag, not an SSA field owner.
const fieldManager = "kanz-operator-venue-keys"

// KubeStore is the dev/rig backend: it writes each venue's key set into a Kubernetes
// Secret named venue-<venue>-keys in the configured namespace, with data keys the
// venue adapter's CSI/file mount expects (api_key / api_secret / api_passphrase). It
// reads a Secret's METADATA for presence and never surfaces its Data — the write-only
// guarantee is the interface (no value-returning method) plus this code plus the
// namespace bound (k8s Secret RBAC cannot express presence-without-value).
type KubeStore struct {
	cs        kubernetes.Interface
	namespace string
}

func NewKubeStore(cs kubernetes.Interface, namespace string) *KubeStore {
	return &KubeStore{cs: cs, namespace: namespace}
}

func secretName(venue string) string { return "venue-" + venue + "-keys" }

// SetVenueKeys is a create-or-update in one call: Get, then Create on IsNotFound
// or Update otherwise. This Get→Create/Update sequence is the only path, in both
// tests and production — it is not a fake-clientset workaround. The brief's typed
// server-side Apply was abandoned because client-go v0.31.3's fake clientset does
// not create-on-apply for Secrets; Apply against a fake clientset with no existing
// object fails with:
//
//	secrets "venue-<venue>-keys" not found
//
// It does not read back the existing Secret's value: on update, the object's Data
// is fully replaced by the incoming VenueKeys, never merged with or derived from
// the old value — this preserves the write-only contract.
func (s *KubeStore) SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error {
	data := map[string][]byte{
		"api_key":    []byte(keys.APIKey),
		"api_secret": []byte(keys.APISecret),
	}
	if keys.Passphrase != "" {
		data["api_passphrase"] = []byte(keys.Passphrase)
	}
	name := secretName(venue)

	existing, err := s.cs.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if _, err := s.cs.CoreV1().Secrets(s.namespace).Create(ctx, sec, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("create venue secret %s: %w", name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get venue secret %s: %w", name, err)
	}

	// Overwrite Data from the incoming keys only — the existing value is never read.
	existing.Type = corev1.SecretTypeOpaque
	existing.Data = data
	if _, err := s.cs.CoreV1().Secrets(s.namespace).Update(ctx, existing, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("update venue secret %s: %w", name, err)
	}
	return nil
}

func (s *KubeStore) ListVenues(ctx context.Context) ([]VenueStatus, error) {
	out := make([]VenueStatus, 0, len(KnownVenues()))
	for _, v := range KnownVenues() {
		// Get is used only for existence — the returned object's Data is never read.
		_, err := s.cs.CoreV1().Secrets(s.namespace).Get(ctx, secretName(v), metav1.GetOptions{})
		switch {
		case err == nil:
			out = append(out, VenueStatus{Venue: v, Configured: true})
		case apierrors.IsNotFound(err):
			out = append(out, VenueStatus{Venue: v, Configured: false})
		default:
			return nil, fmt.Errorf("get venue secret %s: %w", secretName(v), err)
		}
	}
	return out, nil
}
