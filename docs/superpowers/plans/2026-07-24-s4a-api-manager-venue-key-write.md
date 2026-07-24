# S4a — API Manager (operator-plane venue-key write path) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** give `root@universe` an operator-plane surface to register an exchange API key set into its secret location (a write-only `SecretStore` with a k8s-Secret rig impl and a dependency-free Vault KV-v2 prod impl), surfaced as a masked TUI pane.

**Architecture:** additive `operator.v1` RPCs (`SetVenueKeys`/`ListVenueKeys`) → a new `services/operator/internal/secrets` package holding a value-blind `Store` interface + two backends → gRPC handlers on the existing operator `Server` (mirroring the S3 `nodeOps` shape) → a bounded RBAC Role → a `universe` API-Manager pane with a masked key-entry form. Write-only end to end: no path returns key material.

**Tech Stack:** Go, buf/protobuf, client-go v0.31.3 (fake clientset + server-side Apply), `net/http` + `httptest` (Vault KV-v2), Bubble Tea TUI, `yaml.v3` (arch guard).

## Global Constraints

- **Write-only invariant.** The `secrets.Store` interface has NO method returning key material; `ListVenues` returns only `{venue, configured}`. The operator NEVER logs or returns an api_key/secret/passphrase on any path (test-enforced).
- **Known venues + required fields, one source of truth** in `secrets`: `okx` requires api_key + api_secret + **passphrase**; `binance` requires api_key + api_secret and **rejects a passphrase**.
- **Secret location** (k8s backend): Secret `venue-<venue>-keys`, `type: Opaque`, namespace `kanz-services` (default, config-overridable), data keys **`api_key` / `api_secret` / `api_passphrase`** (the SecretProviderClass `secretKey` convention).
- **Vault location** (prod backend): KV-v2 PUT to `{VAULT_ADDR}/v1/kv/data/kanz/venue-<venue>`, `{"data":{"api_key":…,"api_secret":…,"api_passphrase":…}}`, `X-Vault-Token` header. **Dependency-free** (`net/http`, no `hashicorp/vault` SDK).
- **RBAC** `operator-venue-secret-writer`: namespaced to `kanz-services`, `secrets` resource only, verbs `create,patch,get,list` — never cluster-scoped, no other verb/resource.
- **No `crypto/ssh`** (HTTP + k8s API only); `TestNoSSHPlane` stays green.
- **TUI:** secret + passphrase fields render masked (`•`); the typed secret is cleared from the model after submit; `render` stays pure.
- **Env (Windows, no make):** build/test from `C:\Users\root\Desktop\eighred-kanz\kanz`; `buf generate` from `kanz-schemas`; `GOFLAGS=-mod=mod`; `GOTMPDIR="$(pwd)/.gotmp"` for `go test` (create `.gotmp` first — App Control blocks %TEMP% test binaries; a lone transient FAIL that re-runs green is that flake). `gofmt -w`; `go vet ./...`.
- Additive-only proto; `buf breaking` clean. `govulncheck ./...` 0-reachable.

---

## File Structure

- `kanz-schemas/proto/operator/v1/operator.proto` — +2 RPCs, +5 messages (Task 1).
- `kanz/services/operator/internal/secrets/secrets.go` — `VenueKeys`, `VenueStatus`, `Store`, known-venues + `ValidateVenueKeys` (Task 2).
- `kanz/services/operator/internal/secrets/kube.go` + `kube_test.go` — k8s-Secret backend (Task 2).
- `kanz/services/operator/internal/secrets/vault.go` + `vault_test.go` — Vault KV-v2 backend (Task 3).
- `kanz/services/operator/internal/grpcsrv/server.go` + `server_test.go` — handlers + `WithSecrets` (Task 4).
- `kanz/services/operator/internal/config/config.go` + `kanz/services/operator/cmd/operator/main.go` — backend wiring (Task 4).
- `kanz/infra/deploy/operator-deploy.yaml` — Role + RoleBinding (Task 5).
- `kanz/test/arch/operator_venue_secret_rbac_test.go` — bound guard (Task 5).
- `kanz/cmd/universe/{source.go,model.go,view.go}` + tests — API pane read side (Task 6).
- `kanz/cmd/universe/{keyform.go,model.go,view.go}` + tests — key-entry write form (Task 7).

---

### Task 1: proto — SetVenueKeys + ListVenueKeys

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerate: the `kanz-schemas-go` SDK via `buf generate`

**Interfaces:**
- Produces: `operatorpb.{SetVenueKeysRequest,SetVenueKeysResponse,ListVenueKeysRequest,ListVenueKeysResponse,VenueKeyStatus}` and the two client/server RPC methods, consumed by Tasks 4 and 6.

- [ ] **Step 1: Add the RPCs** to `service OperatorService`, after `SetNodeRegion` (proto line 60):

```proto
  // SetVenueKeys registers an exchange API key set (api_key, api_secret, and for
  // OKX a passphrase) into the venue's secret location. Write-only: the operator
  // can set a venue's keys but can never read them back. The value never appears
  // in a response, and the platform's venue adapters consume it via their existing
  // CSI/Vault mount.
  rpc SetVenueKeys(SetVenueKeysRequest) returns (SetVenueKeysResponse);

  // ListVenueKeys reports which venues have a key set configured — presence ONLY,
  // never the key material.
  rpc ListVenueKeys(ListVenueKeysRequest) returns (ListVenueKeysResponse);
```

- [ ] **Step 2: Add the messages** at the end of the file (after `SetNodeRegionResponse`, proto line 169):

```proto
message SetVenueKeysRequest {
  string venue      = 1; // "okx" | "binance"
  string api_key    = 2;
  string api_secret = 3;
  string passphrase = 4; // OKX requires it; Binance must leave it empty
}
message SetVenueKeysResponse {}

message ListVenueKeysRequest {}
message ListVenueKeysResponse {
  repeated VenueKeyStatus venues = 1;
}
message VenueKeyStatus {
  string venue      = 1;
  bool   configured = 2; // presence only — NEVER key material
}
```

- [ ] **Step 3: Update the service doc comment** (proto lines 3-7) — change "Further node operations (drain, key material) arrive in later subsystems" to note key material is now `SetVenueKeys` (S4a). Keep it one line.

- [ ] **Step 4: Regenerate the SDK.**

Run (from `C:\Users\root\Desktop\eighred-kanz\kanz-schemas`): `buf generate`
Expected: `gen/go/operator/v1/operator.pb.go` + `operator_grpc.pb.go` regenerate with the new types and `OperatorServiceClient.SetVenueKeys`/`ListVenueKeys` + `OperatorServiceServer` methods.

- [ ] **Step 5: Verify it builds.**

Run (from `kanz`): `GOFLAGS=-mod=mod go build ./...`
Expected: clean (the generated `UnimplementedOperatorServiceServer` supplies the new methods, so the operator still compiles before Task 4).

Also run `buf breaking` if wired, or confirm additive-only by inspection.

- [ ] **Step 6: Commit.**

```bash
git add kanz-schemas/proto/operator/v1/operator.proto kanz-schemas/gen/go/operator/v1/
git commit -m "feat(schemas): operator.v1 SetVenueKeys/ListVenueKeys (API Manager write path)"
```

---

### Task 2: secrets package — Store interface, validation, k8s-Secret backend

**Files:**
- Create: `kanz/services/operator/internal/secrets/secrets.go`
- Create: `kanz/services/operator/internal/secrets/kube.go`
- Test: `kanz/services/operator/internal/secrets/kube_test.go`

**Interfaces:**
- Produces: `secrets.Store` interface, `secrets.VenueKeys`, `secrets.VenueStatus`, `secrets.KnownVenues() []string`, `secrets.ValidateVenueKeys(venue string, k VenueKeys) error`, `secrets.NewKubeStore(cs kubernetes.Interface, namespace string) *KubeStore` — consumed by Task 4 (handlers) and Task 3 (Vault impl reuses the interface + validation).

- [ ] **Step 1: Write `secrets.go`** — the interface, types, and validation (single source of truth):

```go
// Package secrets is the operator's write-only venue-credential surface. It puts an
// exchange API key set into the venue's secret location (a Kubernetes Secret in dev,
// Vault KV-v2 in production) so the platform's venue adapters can mount it. It is
// WRITE-ONLY BY DESIGN: no method returns key material — presence is the only read,
// because an exchange key moves real capital and must never be reachable for read
// from a request path.
package secrets

import (
	"context"
	"fmt"
	"sort"
)

// VenueKeys is a candidate exchange key set. It is passed to a backend and never
// returned by any Store method.
type VenueKeys struct {
	APIKey     string
	APISecret  string
	Passphrase string
}

// VenueStatus is the value-blind presence report for one venue.
type VenueStatus struct {
	Venue      string
	Configured bool
}

// Store is the write-only venue-credential surface. It has NO method returning key
// material — ListVenues reports presence only. Both the k8s and Vault backends satisfy it.
type Store interface {
	SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error
	ListVenues(ctx context.Context) ([]VenueStatus, error)
}

// venueSpec records a known venue and whether the exchange uses an API passphrase
// (OKX does; Binance does not). This map is the single source of truth for both
// validation and the presence listing.
var venueSpecs = map[string]struct{ needsPassphrase bool }{
	"okx":     {needsPassphrase: true},
	"binance": {needsPassphrase: false},
}

// KnownVenues returns the supported venue ids, sorted (stable for the presence list).
func KnownVenues() []string {
	out := make([]string, 0, len(venueSpecs))
	for v := range venueSpecs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ValidateVenueKeys rejects an unknown venue, a missing api_key/api_secret, a
// passphrase supplied for a venue that has none, or a missing passphrase for one that
// requires it. It is called at the handler before any backend write, so a malformed
// key set never reaches a store.
func ValidateVenueKeys(venue string, k VenueKeys) error {
	spec, ok := venueSpecs[venue]
	if !ok {
		return fmt.Errorf("unknown venue %q (known: %v)", venue, KnownVenues())
	}
	if k.APIKey == "" || k.APISecret == "" {
		return fmt.Errorf("venue %q: api_key and api_secret are required", venue)
	}
	if spec.needsPassphrase && k.Passphrase == "" {
		return fmt.Errorf("venue %q requires an api passphrase", venue)
	}
	if !spec.needsPassphrase && k.Passphrase != "" {
		return fmt.Errorf("venue %q takes no passphrase", venue)
	}
	return nil
}
```

- [ ] **Step 2: Write the failing test `kube_test.go`.**

```go
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
```

- [ ] **Step 3: Run it — expect FAIL** (`NewKubeStore` undefined).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/secrets/... -run TestKubeStore -v`
Expected: build failure / FAIL.

- [ ] **Step 4: Write `kube.go`** — server-side Apply, no value read-back:

```go
package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
)

// fieldManager owns the venue-key Secret fields under server-side apply, so a
// re-apply updates in place rather than colliding.
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

func (s *KubeStore) SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error {
	data := map[string][]byte{
		"api_key":    []byte(keys.APIKey),
		"api_secret": []byte(keys.APISecret),
	}
	if keys.Passphrase != "" {
		data["api_passphrase"] = []byte(keys.Passphrase)
	}
	ac := corev1apply.Secret(secretName(venue), s.namespace).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
	// Apply is create-or-update in one call, with no read-back of the existing value.
	_, err := s.cs.CoreV1().Secrets(s.namespace).Apply(ctx, ac, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	if err != nil {
		return fmt.Errorf("apply venue secret %s: %w", secretName(venue), err)
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
```

> Note: the fake clientset supports `Apply` on typed apply-configurations in client-go v0.31.3. If a fake-specific quirk surfaces (e.g. Apply not creating), fall back to Get→Create/Update in `SetVenueKeys` while keeping the no-value-read contract (Create on IsNotFound, Update otherwise); do NOT weaken the interface.

- [ ] **Step 5: Run — expect PASS** (3 kube tests + confirm `secrets.go` validation compiles).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/secrets/... -v`
Expected: PASS.

- [ ] **Step 6: Add a validation test** (append to a new `secrets_test.go` or `kube_test.go`) proving the required-field matrix, then re-run:

```go
func TestValidateVenueKeys(t *testing.T) {
	cases := []struct {
		name    string
		venue   string
		keys    VenueKeys
		wantErr bool
	}{
		{"okx ok", "okx", VenueKeys{"k", "s", "p"}, false},
		{"okx no passphrase", "okx", VenueKeys{"k", "s", ""}, true},
		{"binance ok", "binance", VenueKeys{"k", "s", ""}, false},
		{"binance with passphrase", "binance", VenueKeys{"k", "s", "p"}, true},
		{"missing secret", "okx", VenueKeys{"k", "", "p"}, true},
		{"unknown venue", "kraken", VenueKeys{"k", "s", ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateVenueKeys(c.venue, c.keys)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateVenueKeys(%q) err=%v, wantErr=%v", c.venue, err, c.wantErr)
			}
		})
	}
}
```

- [ ] **Step 7: gofmt, then commit.**

```bash
gofmt -w services/operator/internal/secrets/
git add kanz/services/operator/internal/secrets/
git commit -m "feat(operator): secrets.Store + k8s-Secret venue-key backend (write-only)"
```

---

### Task 3: secrets package — Vault KV-v2 backend (dependency-free)

**Files:**
- Create: `kanz/services/operator/internal/secrets/vault.go`
- Test: `kanz/services/operator/internal/secrets/vault_test.go`

**Interfaces:**
- Produces: `secrets.NewVaultStore(addr, token string) (*VaultStore, error)` satisfying `secrets.Store` — consumed by Task 4's `main.go` wiring.

- [ ] **Step 1: Write the failing test `vault_test.go`** against an `httptest` Vault:

```go
package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVaultStoreSetVenueKeysPuts(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Vault-Token")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	s, err := NewVaultStore(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewVaultStore: %v", err)
	}
	if err := s.SetVenueKeys(context.Background(), "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}); err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if gotPath != "/v1/kv/data/kanz/venue-okx" {
		t.Errorf("path = %q, want /v1/kv/data/kanz/venue-okx", gotPath)
	}
	if gotToken != "tok" {
		t.Errorf("token header = %q, want tok", gotToken)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data["api_key"] != "k" || data["api_secret"] != "s" || data["api_passphrase"] != "p" {
		t.Errorf("body data = %v, want the three fields", data)
	}
}

func TestVaultStoreSetVenueKeysErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer srv.Close()
	s, _ := NewVaultStore(srv.URL, "tok")
	if err := s.SetVenueKeys(context.Background(), "okx", VenueKeys{APIKey: "k", APISecret: "s", Passphrase: "p"}); err == nil {
		t.Fatal("want error on 403")
	}
}

func TestVaultStoreListVenues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// KV-v2 LIST of kv/metadata/kanz — returns key names under kanz/, never values.
		_, _ = w.Write([]byte(`{"data":{"keys":["venue-okx","risk-engine","oms"]}}`))
	}))
	defer srv.Close()
	s, _ := NewVaultStore(srv.URL, "tok")
	got, err := s.ListVenues(context.Background())
	if err != nil {
		t.Fatalf("ListVenues: %v", err)
	}
	byVenue := map[string]bool{}
	for _, v := range got {
		byVenue[v.Venue] = v.Configured
	}
	if !byVenue["okx"] || byVenue["binance"] {
		t.Errorf("presence = %v, want okx configured, binance not", byVenue)
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`NewVaultStore` undefined).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/secrets/... -run TestVault -v`
Expected: FAIL.

- [ ] **Step 3: Write `vault.go`** — hand-rolled KV-v2, no vendor SDK:

```go
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VaultStore is the production backend: a minimal, dependency-free Vault KV-v2 writer
// (matching the codebase's hand-rolled no-vendor-SDK HTTP convention). It writes each
// venue's key set to kv/data/kanz/venue-<venue> and derives presence from a KV-v2 LIST
// of kv/metadata/kanz, which returns key NAMES, not values — so it is value-blind by
// construction, unlike the k8s backend.
type VaultStore struct {
	addr  string
	token string
	httpc *http.Client
}

func NewVaultStore(addr, token string) (*VaultStore, error) {
	if addr == "" || token == "" {
		return nil, fmt.Errorf("vault store: addr and token are required")
	}
	return &VaultStore{
		addr:  strings.TrimRight(addr, "/"),
		token: token,
		httpc: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (s *VaultStore) SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error {
	data := map[string]string{"api_key": keys.APIKey, "api_secret": keys.APISecret}
	if keys.Passphrase != "" {
		data["api_passphrase"] = keys.Passphrase
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	url := s.addr + "/v1/kv/data/kanz/venue-" + venue
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("vault put venue-%s: %w", venue, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status alone — never any request body carrying key material — is surfaced.
		return fmt.Errorf("vault put venue-%s: status %d", venue, resp.StatusCode)
	}
	return nil
}

func (s *VaultStore) ListVenues(ctx context.Context) ([]VenueStatus, error) {
	url := s.addr + "/v1/kv/metadata/kanz?list=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", s.token)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault list kanz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vault list kanz: status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("vault list kanz: decode: %w", err)
	}
	present := map[string]bool{}
	for _, k := range out.Data.Keys {
		present[strings.TrimSuffix(k, "/")] = true
	}
	res := make([]VenueStatus, 0, len(KnownVenues()))
	for _, v := range KnownVenues() {
		res = append(res, VenueStatus{Venue: v, Configured: present["venue-"+v]})
	}
	return res, nil
}
```

- [ ] **Step 4: Run — expect PASS** (3 vault tests).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/secrets/... -v`
Expected: PASS (all secrets tests: validation + kube + vault).

- [ ] **Step 5: gofmt, commit.**

```bash
gofmt -w services/operator/internal/secrets/
git add kanz/services/operator/internal/secrets/vault.go kanz/services/operator/internal/secrets/vault_test.go
git commit -m "feat(operator): dependency-free Vault KV-v2 venue-key backend"
```

---

### Task 4: gRPC handlers + backend wiring

**Files:**
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`
- Modify: `kanz/services/operator/internal/config/config.go`
- Modify: `kanz/services/operator/cmd/operator/main.go`

**Interfaces:**
- Consumes: `secrets.Store`, `secrets.VenueKeys`, `secrets.ValidateVenueKeys`, `secrets.VenueStatus` (Task 2); the proto types (Task 1).
- Produces: `Server.WithSecrets(secrets.Store) *Server`; `SetVenueKeys`/`ListVenueKeys` handlers.

- [ ] **Step 1: Write failing handler tests** in `server_test.go`. Extend the EXISTING test stub file with a `stubSecretStore` (record the last `SetVenueKeys` args; configurable error; a fixed `ListVenues`). Mirror how `stubNodeOps` is used for the Cordon tests.

```go
type stubSecretStore struct {
	venue   string
	keys    secrets.VenueKeys
	setErr  error
	listOut []secrets.VenueStatus
	logged  []string // anything the store "would log" — asserted empty of key material
}

func (s *stubSecretStore) SetVenueKeys(_ context.Context, venue string, k secrets.VenueKeys) error {
	s.venue, s.keys = venue, k
	return s.setErr
}
func (s *stubSecretStore) ListVenues(context.Context) ([]secrets.VenueStatus, error) {
	return s.listOut, nil
}

func TestSetVenueKeysDelegates(t *testing.T) {
	st := &stubSecretStore{}
	srv := New(stubReader{}).WithSecrets(st)
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{
		Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p",
	})
	if err != nil {
		t.Fatalf("SetVenueKeys: %v", err)
	}
	if st.venue != "okx" || st.keys.APIKey != "k" || st.keys.APISecret != "s" || st.keys.Passphrase != "p" {
		t.Errorf("store got venue=%q keys=%+v, want the request's values", st.venue, st.keys)
	}
}

func TestSetVenueKeysNilStoreUnimplemented(t *testing.T) {
	srv := New(stubReader{}) // no WithSecrets
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", status.Code(err))
	}
}

func TestSetVenueKeysValidationInvalidArgument(t *testing.T) {
	st := &stubSecretStore{}
	srv := New(stubReader{}).WithSecrets(st)
	// binance with a passphrase is rejected before the store is touched.
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "binance", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
	if st.venue != "" {
		t.Errorf("store must NOT be called on a validation failure")
	}
}

func TestSetVenueKeysWriteErrorInternal(t *testing.T) {
	st := &stubSecretStore{setErr: errors.New("backend down")}
	srv := New(stubReader{}).WithSecrets(st)
	_, err := srv.SetVenueKeys(context.Background(), &operatorpb.SetVenueKeysRequest{Venue: "okx", ApiKey: "k", ApiSecret: "s", Passphrase: "p"})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

func TestListVenueKeysMapsPresence(t *testing.T) {
	st := &stubSecretStore{listOut: []secrets.VenueStatus{{Venue: "okx", Configured: true}, {Venue: "binance", Configured: false}}}
	srv := New(stubReader{}).WithSecrets(st)
	resp, err := srv.ListVenueKeys(context.Background(), &operatorpb.ListVenueKeysRequest{})
	if err != nil {
		t.Fatalf("ListVenueKeys: %v", err)
	}
	if len(resp.GetVenues()) != 2 || resp.GetVenues()[0].GetVenue() != "okx" || !resp.GetVenues()[0].GetConfigured() {
		t.Errorf("venues = %+v, want okx configured + binance not", resp.GetVenues())
	}
}
```

> Use whatever the existing read-model stub is named (`stubReader` here is a placeholder — match the file's existing reader stub used by the ListNodes tests). Add `"errors"` and the `secrets` import as needed.

- [ ] **Step 2: Run — expect FAIL** (`WithSecrets`/handlers undefined).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/grpcsrv/... -run Venue -v`
Expected: FAIL.

- [ ] **Step 3: Add the field, builder, and handlers to `server.go`.** Add `secrets secrets.Store` to the `Server` struct; import `github.com/kanz-eng/kanz/services/operator/internal/secrets`. Add:

```go
// WithSecrets attaches the write-only venue-credential surface (builder style).
func (s *Server) WithSecrets(store secrets.Store) *Server { s.secrets = store; return s }

func (s *Server) SetVenueKeys(ctx context.Context, req *operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error) {
	if s.secrets == nil {
		return nil, status.Error(codes.Unimplemented, "API management not configured")
	}
	keys := secrets.VenueKeys{APIKey: req.GetApiKey(), APISecret: req.GetApiSecret(), Passphrase: req.GetPassphrase()}
	if err := secrets.ValidateVenueKeys(req.GetVenue(), keys); err != nil {
		// The error names the venue and the field rule — NEVER the key material.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.secrets.SetVenueKeys(ctx, req.GetVenue(), keys); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &operatorpb.SetVenueKeysResponse{}, nil
}

func (s *Server) ListVenueKeys(ctx context.Context, _ *operatorpb.ListVenueKeysRequest) (*operatorpb.ListVenueKeysResponse, error) {
	if s.secrets == nil {
		return nil, status.Error(codes.Unimplemented, "API management not configured")
	}
	vs, err := s.secrets.ListVenues(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.VenueKeyStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, &operatorpb.VenueKeyStatus{Venue: v.Venue, Configured: v.Configured})
	}
	return &operatorpb.ListVenueKeysResponse{Venues: out}, nil
}
```

> The handler deliberately builds the `VenueKeys` and passes it straight to validate+store; it never logs the request. Do NOT add a debug log of the request anywhere in this path.

- [ ] **Step 4: Run — expect PASS** (5 venue handler tests + the existing suite).

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./services/operator/internal/grpcsrv/... -v`
Expected: PASS.

- [ ] **Step 5: Wire config + main.go** (build-verified glue; no unit test — the handler is the tested seam).

In `config.go`, add fields + loads:
```go
	// Venue-key write path (S4a). Empty SecretBackend ⇒ SetVenueKeys is unconfigured
	// (Unimplemented). "kube" writes k8s Secrets; "vault" writes Vault KV-v2.
	SecretBackend        string // OPERATOR_SECRET_BACKEND: "" | "kube" | "vault"
	VenueSecretNamespace string // OPERATOR_VENUE_SECRET_NAMESPACE (kube backend), default kanz-services
	VaultAddr            string // VAULT_ADDR (vault backend)
	VaultToken           string // from VAULT_TOKEN_FILE (preferred) or VAULT_TOKEN
```
In `Load()`:
```go
		SecretBackend:        os.Getenv("OPERATOR_SECRET_BACKEND"),
		VenueSecretNamespace: envOr("OPERATOR_VENUE_SECRET_NAMESPACE", "kanz-services"),
		VaultAddr:            os.Getenv("VAULT_ADDR"),
		VaultToken:           readTokenOr("VAULT_TOKEN_FILE", "VAULT_TOKEN"),
```
Add a small `readTokenOr(fileEnv, valueEnv string) string` helper (read the file at `$fileEnv` if set and non-empty, else `$valueEnv`) — mirror `main.go`'s `namespaceOr` file-read style, `strings.TrimSpace`. Update the package doc comment (config.go:1-4) — it currently says "no write target, no credential"; note S4a adds the venue-key backend config.

In `main.go`, after the `WithNodeOps` line (main.go:93), wire the backend:
```go
	switch cfg.SecretBackend {
	case "kube":
		srv = srv.WithSecrets(secrets.NewKubeStore(cs, cfg.VenueSecretNamespace))
		logger.Info("venue-key backend: kubernetes secrets", "namespace", cfg.VenueSecretNamespace)
	case "vault":
		vs, verr := secrets.NewVaultStore(cfg.VaultAddr, cfg.VaultToken)
		if verr != nil {
			logger.Error("vault venue-key backend init failed", "err", verr)
			os.Exit(2)
		}
		srv = srv.WithSecrets(vs)
		logger.Info("venue-key backend: vault", "addr", cfg.VaultAddr)
	default:
		logger.Warn("no OPERATOR_SECRET_BACKEND — SetVenueKeys disabled")
	}
```
Add the `secrets` import.

- [ ] **Step 6: Build + vet.**

Run: `GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./services/operator/...`
Expected: clean.

- [ ] **Step 7: gofmt, commit.**

```bash
gofmt -w services/operator/
git add kanz/services/operator/internal/grpcsrv/ kanz/services/operator/internal/config/config.go kanz/services/operator/cmd/operator/main.go
git commit -m "feat(operator): SetVenueKeys/ListVenueKeys handlers + backend wiring"
```

---

### Task 5: RBAC + arch guard

**Files:**
- Modify: `kanz/infra/deploy/operator-deploy.yaml`
- Create: `kanz/test/arch/operator_venue_secret_rbac_test.go`

**Interfaces:**
- Consumes: the operator ServiceAccount already declared in `operator-deploy.yaml`.
- Produces: `operator-venue-secret-writer` Role + RoleBinding; `TestOperatorVenueSecretRoleIsBounded`.

- [ ] **Step 1: Read the existing operator RBAC** in `kanz/infra/deploy/operator-deploy.yaml` (the `operator-node-writer` ClusterRole/binding + the operator ServiceAccount name/namespace) and the existing guard `kanz/test/arch/` that bounds the node-writer role (the `TestOperatorNodeWriterRoleIsBounded` test + its `decodeOperatorManifest`/`roleDoc` helper). This task mirrors both.

- [ ] **Step 2: Add the Role + RoleBinding** to `operator-deploy.yaml` (namespaced to `kanz-services`, bound to the operator SA in its namespace):

```yaml
---
# operator-venue-secret-writer: the S4a API-Manager write path. The operator writes
# venue API-key Secrets into kanz-services (where the venue adapters mount them). It is
# bounded to the secrets resource in this one namespace — never cluster-scoped, never
# another resource or verb. get/list are the presence read (k8s Secret RBAC cannot
# express presence-without-value; the write-only guarantee is the code + this bound).
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: operator-venue-secret-writer
  namespace: kanz-services
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "patch", "get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: operator-venue-secret-writer
  namespace: kanz-services
subjects:
- kind: ServiceAccount
  name: <OPERATOR_SA_NAME>          # match the SA in the operator Deployment
  namespace: <OPERATOR_SA_NAMESPACE> # e.g. kanz-operator
roleRef:
  kind: Role
  name: operator-venue-secret-writer
  apiGroup: rbac.authorization.k8s.io
```

Fill `<OPERATOR_SA_NAME>`/`<OPERATOR_SA_NAMESPACE>` from the values already in the file.

- [ ] **Step 3: Write the failing guard** `operator_venue_secret_rbac_test.go`, mirroring the node-writer guard's decode helper:

```go
package arch

import "testing"

// TestOperatorVenueSecretRoleIsBounded fails the build if the S4a venue-secret Role
// grants anything beyond {create,patch,get,list} on the secrets resource in a single
// namespace — a wider grant (delete, a different resource, or a ClusterRole) would let
// the API-Manager write path reach past venue credentials.
func TestOperatorVenueSecretRoleIsBounded(t *testing.T) {
	roles := decodeOperatorManifest(t) // existing helper
	var found bool
	for _, r := range roles {
		if r.Kind != "Role" || r.Metadata.Name != "operator-venue-secret-writer" {
			continue
		}
		found = true
		if r.Metadata.Namespace == "" {
			t.Error("operator-venue-secret-writer must be namespaced, not cluster-scoped")
		}
		allowedVerbs := map[string]bool{"create": true, "patch": true, "get": true, "list": true}
		for _, rule := range r.Rules {
			for _, res := range rule.Resources {
				if res != "secrets" {
					t.Errorf("venue-secret role grants resource %q, want only secrets", res)
				}
			}
			for _, g := range rule.APIGroups {
				if g != "" {
					t.Errorf("venue-secret role grants apiGroup %q, want only core", g)
				}
			}
			for _, v := range rule.Verbs {
				if !allowedVerbs[v] {
					t.Errorf("venue-secret role grants verb %q outside {create,patch,get,list}", v)
				}
			}
		}
	}
	if !found {
		t.Fatal("operator-venue-secret-writer Role not found in operator-deploy.yaml")
	}
}
```

> Match `decodeOperatorManifest`'s actual `roleDoc` field names (Kind, Metadata.Name/Namespace, Rules[].Resources/APIGroups/Verbs). If the helper only decodes ClusterRoles today, widen it to also yield `Role` docs (both share the shape) — do not fork a second decoder.

- [ ] **Step 4: Run — expect PASS** (guard green against the real manifest), then mutation-check.

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./test/arch/... -run VenueSecret -v`
Expected: PASS. Then temporarily add `delete` to the Role's verbs (or `configmaps` to resources), re-run, confirm the guard goes RED, and revert. Record that the mutation was observed to fail.

- [ ] **Step 5: Commit.**

```bash
git add kanz/infra/deploy/operator-deploy.yaml kanz/test/arch/operator_venue_secret_rbac_test.go
git commit -m "feat(operator): bounded venue-secret RBAC + arch guard"
```

---

### Task 6: TUI — API Manager pane (read/presence)

**Files:**
- Modify: `kanz/cmd/universe/source.go`
- Modify: `kanz/cmd/universe/model.go`
- Modify: `kanz/cmd/universe/view.go`
- Test: `kanz/cmd/universe/{source_test.go,model_test.go,view_test.go}`

**Interfaces:**
- Consumes: `operatorpb.ListVenueKeysRequest`, `SetVenueKeysRequest` (Task 1).
- Produces: `nodeSource.setVenueKeys`/`listVenueKeys`, `venueKeys` struct, `venueRow` struct, `paneAPI` — consumed by Task 7.

- [ ] **Step 1: Widen `nodeSource` + grpcSource** in `source.go`. Add to the `nodeSource` interface (after `setRegion` from S3b):
```go
	setVenueKeys(ctx context.Context, venue string, keys venueKeys) error
	listVenueKeys(ctx context.Context) ([]venueRow, error)
```
Add the payload type and the grpcSource impls (mirror `setRegion`/`drain` at source.go):
```go
// venueKeys is the write payload for SetVenueKeys — the typed secret is passed through
// to the RPC and never retained on a model field (the S2a key-bytes discipline).
type venueKeys struct{ apiKey, apiSecret, passphrase string }

func (g *grpcSource) setVenueKeys(ctx context.Context, venue string, keys venueKeys) error {
	_, err := g.client.SetVenueKeys(ctx, &operatorpb.SetVenueKeysRequest{
		Venue: venue, ApiKey: keys.apiKey, ApiSecret: keys.apiSecret, Passphrase: keys.passphrase,
	})
	return err
}

func (g *grpcSource) listVenueKeys(ctx context.Context) ([]venueRow, error) {
	resp, err := g.client.ListVenueKeys(ctx, &operatorpb.ListVenueKeysRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]venueRow, 0, len(resp.GetVenues()))
	for _, v := range resp.GetVenues() {
		out = append(out, venueRow{venue: v.GetVenue(), configured: v.GetConfigured()})
	}
	return out, nil
}
```
Add `venueRow` to model.go (Step 2). Add `venues []venueRow` to `fetchMsg` (source.go) and populate it best-effort in `grpcSource.fetch` (like `provisions` — a transient error leaves the strip empty, never fails the poll):
```go
	venues, _ := g.listVenueKeys(ctx)
	// ... in the returned fetchMsg: venues: venues,
```
Update the stub `nodeSource` in `source_test.go`/`model_test.go` with both new methods (record the last `setVenueKeys` args; return a fixed `listVenueKeys`).

- [ ] **Step 2: Model state** in `model.go`. Add the row type and pane:
```go
// venueRow is one row of the API-Manager pane — presence only, never key material.
type venueRow struct {
	venue      string
	configured bool
}
```
Add `paneAPI` to the pane enum (after `paneClusters`). Add to `model`: `venues []venueRow` and `apiSelected int`. In `Update`'s `fetchMsg` case, set `m.venues = msg.venues` and clamp `m.apiSelected` with the existing `clampSelected(m.apiSelected, len(m.venues))`. Extend the `tab` handler to cycle nodes→clusters→api→nodes. Add `up`/`down` for `paneAPI` moving `m.apiSelected` (guard bounds like the nodes pane).

- [ ] **Step 3: Write failing tests** (`model_test.go`, `view_test.go`): tab cycles into `paneAPI`; a `fetchMsg` with venues populates the pane and clamps selection; `renderAPI` lists both venues with a configured/not marker and never prints any key material (there is none to print — assert the rows show `configured`/`not set`). Run — expect FAIL.

- [ ] **Step 4: Render** in `view.go`. Add a `renderAPI()` (mirror `renderClusters`) and route it from `render()`'s pane switch:
```go
func (m model) renderAPI() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("API MANAGER") + "\n")
	b.WriteString(fmt.Sprintf("   %-12s %-14s\n", "VENUE", "KEYS"))
	for i, v := range m.venues {
		state := styleDim.Render("— not set")
		if v.configured {
			state = styleReady.Render("✓ configured")
		}
		marker := "  "
		if i == m.apiSelected {
			marker = styleSelected.Render("▸ ")
		}
		b.WriteString(marker + fmt.Sprintf("%-12s %s\n", v.venue, state))
	}
	return b.String()
}
```
Add an `[k] set keys` hint to `renderStatus` when `m.active == paneAPI`. Run — expect PASS. gofmt.

- [ ] **Step 5: Commit.**

```bash
gofmt -w cmd/universe/
git add kanz/cmd/universe/source.go kanz/cmd/universe/model.go kanz/cmd/universe/view.go kanz/cmd/universe/source_test.go kanz/cmd/universe/model_test.go kanz/cmd/universe/view_test.go
git commit -m "feat(universe): API Manager pane — venue-key presence (read)"
```

---

### Task 7: TUI — masked key-entry form (write)

**Files:**
- Create: `kanz/cmd/universe/keyform.go`
- Modify: `kanz/cmd/universe/model.go`
- Modify: `kanz/cmd/universe/view.go`
- Test: `kanz/cmd/universe/{keyform_test.go,model_test.go}`

**Interfaces:**
- Consumes: `venueKeys`, `venueRow`, `paneAPI`, `nodeSource.setVenueKeys` (Task 6).

- [ ] **Step 1: Write `keyform.go`** — a masked form scoped to a chosen venue (mirror `addForm` in form.go, but with per-field masking and a fixed venue):
```go
package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// keyField is one labelled input; masked fields render as bullets so a shared screen
// never shows the secret.
type keyField struct {
	key, label, value string
	masked            bool
}

// keyForm is the venue key-entry form: scoped to one venue (chosen from the pane), it
// collects api_key, api_secret, and — for OKX — a passphrase. Pure state; render() is
// pure so keyform_test drives it with no TTY.
type keyForm struct {
	venue   string
	fields  []keyField
	focused int
}

func newKeyForm(venue string) keyForm {
	fields := []keyField{
		{key: "api_key", label: "API Key"},
		{key: "api_secret", label: "API Secret", masked: true},
	}
	if venue == "okx" {
		fields = append(fields, keyField{key: "passphrase", label: "Passphrase", masked: true})
	}
	return keyForm{venue: venue, fields: fields}
}

func (f keyForm) value(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return fl.value
		}
	}
	return ""
}

func (f keyForm) key(msg tea.KeyMsg) keyForm {
	if msg.Type == tea.KeyRunes {
		f.fields[f.focused].value += string(msg.Runes)
	}
	return f
}

func (f keyForm) backspace() keyForm {
	v := f.fields[f.focused].value
	if v != "" {
		r := []rune(v)
		f.fields[f.focused].value = string(r[:len(r)-1])
	}
	return f
}

func (f keyForm) next() keyForm { f.focused = (f.focused + 1) % len(f.fields); return f }
func (f keyForm) prev() keyForm { f.focused = (f.focused - 1 + len(f.fields)) % len(f.fields); return f }

func (f keyForm) render() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Set API Keys — "+f.venue) + "\n\n")
	for i, fl := range f.fields {
		shown := fl.value
		if fl.masked {
			shown = strings.Repeat("•", len([]rune(fl.value)))
		}
		label := fmt.Sprintf("%-12s", fl.label+":")
		line := fmt.Sprintf("  %s %s", label, shown)
		if i == f.focused {
			line = styleFocused.Render("▸ "+label) + " " + shown + "_"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + styleDim.Render("[tab] next  [enter] save  [esc] cancel"))
	return b.String()
}
```

- [ ] **Step 2: Model wiring** in `model.go`. Add `showKeyForm bool`, `keyForm keyForm`, `keyFormErr error`. Gate key input: when `m.showKeyForm`, route to a new `updateKeyForm(msg)` BEFORE the main switch (mirror `updateForm`, model.go:174). In the main switch add a `k` case for `paneAPI`: if a venue row is selected, `m.showKeyForm = true; m.keyForm = newKeyForm(m.venues[m.apiSelected].venue); m.keyFormErr = nil` (guard `m.apiSelected` against `len(m.venues)`). `updateKeyForm`: Esc cancels+clears; Tab/ShiftTab next/prev; Backspace/Runes edit; Enter → `submitKeyForm()`. Add:
```go
// submitKeyForm fires SetVenueKeys off the UI thread, then the typed secret leaves the
// model — the form is reset regardless of outcome-in-flight, and the result closes it.
func (m model) submitKeyForm() (tea.Model, tea.Cmd) {
	venue := m.keyForm.venue
	keys := venueKeys{
		apiKey:     m.keyForm.value("api_key"),
		apiSecret:  m.keyForm.value("api_secret"),
		passphrase: m.keyForm.value("passphrase"),
	}
	src := m.src
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		return keyFormResultMsg{err: src.setVenueKeys(ctx, venue, keys)}
	}
}

type keyFormResultMsg struct{ err error }
```
In `Update`, handle `keyFormResultMsg`: on `err == nil` → `m.showKeyForm = false; m.keyForm = keyForm{}` (clears the typed secret); on error → `m.keyFormErr = msg.err` and also clear the secret fields (`m.keyForm = keyForm{}` after capturing? — keep it simple: on error, reset the form's secret fields but keep it open with the error; simplest and safe is to close the form and surface the error in the status line via `m.actionErr`, so the secret never lingers). Choose ONE: **on any result, clear `m.keyForm` (drop the typed secret); on error set `m.actionErr` so the operator sees it and can reopen.** Document the choice in a comment.

- [ ] **Step 3: Render the form** in `view.go`'s `render()` — when `m.showKeyForm`, return `m.keyForm.render()` (+ `m.keyFormErr`/`actionErr` line if set), BEFORE the pane switch, mirroring the `m.showForm` branch (view.go:24). Ensure the Add-Node `showForm` branch and this one don't both fire (they can't — different panes/keys).

- [ ] **Step 4: Tests** (`keyform_test.go`, `model_test.go`):
  - `newKeyForm("okx")` has 3 fields incl passphrase; `newKeyForm("binance")` has 2, no passphrase.
  - the api_secret field renders masked (bullets, not the value); assert the raw secret string does NOT appear in `render()`.
  - `k` on a selected venue opens the form scoped to that venue; `k` with zero venues is a safe no-op.
  - typing edits; backspace is rune-safe.
  - Enter returns a cmd; executing it calls the stub's `setVenueKeys` with the venue + typed fields; a successful `keyFormResultMsg` closes the form AND leaves no typed secret on the model (assert `m.keyForm.value("api_secret") == ""`).
  - Esc cancels and clears.
  - `render` pure (double-call identical) for both the pane and the open form.
  Run RED → implement → GREEN. gofmt.

- [ ] **Step 5: Commit.**

```bash
gofmt -w cmd/universe/
git add kanz/cmd/universe/keyform.go kanz/cmd/universe/model.go kanz/cmd/universe/view.go kanz/cmd/universe/keyform_test.go kanz/cmd/universe/model_test.go
git commit -m "feat(universe): masked venue key-entry form (write)"
```

---

### Task 8: whole-slice verification + rig-proof handoff

**Files:** none (verification only; no commit).

- [ ] **Step 1: Build + vet + gofmt.**

Run (from `kanz`): `GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./... && gofmt -l services/ cmd/`
Expected: clean; `gofmt -l` prints nothing.

- [ ] **Step 2: Full test suite.**

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./...`
Expected: all `ok`. Note the counts for `services/operator/internal/secrets`, `.../grpcsrv`, `cmd/universe`.

- [ ] **Step 3: Arch guards.**

Run: `GOTMPDIR="$(pwd)/.gotmp" GOFLAGS=-mod=mod go test ./test/arch/... -v`
Confirm by name: `TestOperatorVenueSecretRoleIsBounded` PASS; `TestNoSSHPlane` PASS (no `crypto/ssh` — this slice is HTTP + k8s); `TestOperatorNodeWriterRoleIsBounded` + `TestOperatorClusterRoleIsNodeReadOnly` unchanged and PASS; S2a provisioner guards PASS.

- [ ] **Step 4: govulncheck.**

Run: `GOFLAGS=-mod=mod govulncheck ./...` (if on PATH).
Expected: 0 reachable; only the 2 known-unreachable advisories (dnsmessage GO-2026-5942, openpgp GO-2026-5932). The dependency-free Vault writer must add no new advisory.

- [ ] **Step 5: Record the rig-proof recipe** (controller runs the manual proof, or hands it to the lead):

k8s backend, FULLY provable on kind (no Vault needed):
1. Deploy the Role/binding: `kubectl apply -f kanz/infra/deploy/operator-deploy.yaml`.
2. Run the operator with `OPERATOR_SECRET_BACKEND=kube` (+ `OPERATOR_VENUE_SECRET_NAMESPACE=kanz-services`); port-forward its gRPC.
3. `universe` → `tab` to API Manager → select `binance` → `k` → enter a dummy api_key/api_secret (secret shows masked) → Enter.
4. Confirm: `kubectl get secret -n kanz-services venue-binance-keys -o jsonpath='{.data.api_key}'` decodes to the typed value; the pane shows `binance ✓ configured` on the next poll.

Infra-gated: the Vault backend's live write (unit-proven vs httptest) and a real venue adapter consuming the Secret (adapters aren't in the rig loop subset).

- [ ] **Step 6: Report** the verification outcome (each command + result) and the rig recipe to the controller for the whole-branch review + board/BRAIN handoff.

---

## Self-Review (author)

- **Spec coverage:** SecretStore seam + both backends (T2, T3) ✓; proto (T1) ✓; handlers + wiring + nil→Unimplemented (T4) ✓; RBAC + guard (T5) ✓; TUI pane + masked form (T6, T7) ✓; write-only/no-key-in-logs (T2 interface, T4 handler, T7 secret-cleared) ✓; rig-provability + infra-gated honesty (T8) ✓.
- **Type consistency:** `secrets.VenueKeys{APIKey,APISecret,Passphrase}` and `secrets.VenueStatus{Venue,Configured}` used identically across T2/T3/T4; TUI `venueKeys{apiKey,apiSecret,passphrase}` and `venueRow{venue,configured}` consistent across T6/T7; proto getters `GetApiKey/GetApiSecret/GetPassphrase/GetVenue/GetConfigured` match the field names in T1.
- **Placeholders:** the RBAC subject SA name/namespace (T5 Step 2) and the reader-stub name (T4 Step 1) are the only "fill from the existing file" spots — each is explicitly flagged with where to read the real value, not left vague.
