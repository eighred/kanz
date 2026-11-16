package pg

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const runtimeBootstrapScript = "../../../tools/bootstrap-testnet-data-plane.sh"

type runtimeVaultFixture struct {
	t        *testing.T
	ctx      context.Context
	client   *http.Client
	address  string
	root     string
	key      *rsa.PrivateKey
	fixtures map[string]*runtimeRoleFixture
	scratch  string
	trust    string
	script   string
}

// Bound child output even before the outer sandbox's file/memory limits apply.
// On failure only the test's generic classification is retained, not this text.
type runtimeBootstrapOutput struct{ bytes.Buffer }

func (output *runtimeBootstrapOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > 64<<10 {
		return 0, io.ErrShortBuffer
	}
	return output.Buffer.Write(data)
}

func (v *runtimeVaultFixture) request(method, path, token string, payload any) (map[string]any, int) {
	v.t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			v.t.Fatal("encode fixture request")
		}
	}
	req, err := http.NewRequestWithContext(v.ctx, method, v.address+"/v1/"+path, &body)
	if err != nil {
		v.t.Fatal("construct fixture request")
	}
	req.Header.Set("X-Vault-Token", token)
	response, err := v.client.Do(req)
	if err != nil {
		v.t.Fatal("fixture Vault request unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		v.t.Fatal("fixture response exceeded its bound")
	}
	result := map[string]any{}
	if len(raw) != 0 && json.Unmarshal(raw, &result) != nil {
		v.t.Fatal("invalid fixture response")
	}
	return result, response.StatusCode
}

func (v *runtimeVaultFixture) write(path string, payload any) map[string]any {
	v.t.Helper()
	result, status := v.request(http.MethodPost, path, v.root, payload)
	if status != http.StatusOK && status != http.StatusNoContent {
		v.t.Fatalf("fixture setup %s refused with status %d", path, status)
	}
	return result
}

func (v *runtimeVaultFixture) record(token, path string, version int, allowed bool) map[string]any {
	v.t.Helper()
	endpoint := "kv/data/" + path
	if version != 0 {
		endpoint += "?version=" + strconv.Itoa(version)
	}
	response, status := v.request(http.MethodGet, endpoint, token, nil)
	if !allowed {
		if status != http.StatusForbidden {
			v.t.Fatalf("credential boundary %s returned %d, want 403", path, status)
		}
		return nil
	}
	if status != http.StatusOK {
		v.t.Fatalf("authorized fixture record %s returned %d", path, status)
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		v.t.Fatal("fixture secret response omitted data")
	}
	fields, ok := data["data"].(map[string]any)
	if !ok {
		v.t.Fatal("fixture secret response omitted fields")
	}
	return fields
}

func (v *runtimeVaultFixture) login(role, namespace, account string, allowed bool) string {
	v.t.Helper()
	claims := map[string]any{
		"iss": "kubernetes/serviceaccount", "sub": "system:serviceaccount:" + namespace + ":" + account,
		"aud": []string{"vault"}, "iat": time.Now().Unix(), "nbf": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(4 * time.Minute).Unix(),
		"kubernetes.io/serviceaccount/namespace":            namespace,
		"kubernetes.io/serviceaccount/service-account.name": account,
		"kubernetes.io/serviceaccount/service-account.uid":  "dummy-" + account,
		"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]string{"name": account, "uid": "dummy-" + account}},
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	encoded, err := json.Marshal(claims)
	if err != nil {
		v.t.Fatal("encode dummy claims")
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(encoded)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, v.key, crypto.SHA256, digest[:])
	if err != nil {
		v.t.Fatal("sign dummy service account")
	}
	response, status := v.request(http.MethodPost, "auth/kubernetes/login", "", map[string]any{"role": role, "jwt": unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)})
	if !allowed {
		if status != http.StatusForbidden {
			v.t.Fatalf("wrong principal/role login returned %d, want 403", status)
		}
		return ""
	}
	if status != http.StatusOK {
		v.t.Fatalf("authorized service account login returned %d", status)
	}
	auth, ok := response["auth"].(map[string]any)
	if !ok {
		v.t.Fatal("fixture login omitted auth")
	}
	token, ok := auth["client_token"].(string)
	if !ok || token == "" {
		v.t.Fatal("fixture login omitted token")
	}
	return token
}

func (v *runtimeVaultFixture) configureKubernetes() {
	v.t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Spec struct {
				Token string `json:"token"`
			} `json:"spec"`
		}
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&request) != nil {
			http.Error(w, "invalid dummy review", 400)
			return
		}
		parts := strings.Split(request.Spec.Token, ".")
		if len(parts) != 3 {
			http.Error(w, "invalid dummy token", 400)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		if err != nil || json.Unmarshal(raw, &claims) != nil {
			http.Error(w, "invalid dummy claims", 400)
			return
		}
		// Vault independently verifies the signature with pem_keys below. This
		// server stands in only for Kubernetes TokenReview, not a real cluster.
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview", "status": map[string]any{
			"authenticated": true, "audiences": []string{"vault"}, "user": map[string]any{"username": claims["sub"], "uid": claims["kubernetes.io/serviceaccount/service-account.uid"], "groups": []string{"system:serviceaccounts"}},
		}})
	}))
	v.t.Cleanup(server.Close)
	publicKey, err := x509.MarshalPKIXPublicKey(&v.key.PublicKey)
	if err != nil {
		v.t.Fatal("encode fixture signing key")
	}
	v.write("auth/kubernetes/config", map[string]any{
		"kubernetes_host": server.URL, "kubernetes_ca_cert": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
		"pem_keys":           []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKey}))},
		"token_reviewer_jwt": "dummy-reviewer", "issuer": "kubernetes/serviceaccount", "disable_iss_validation": false,
	})
}

func (v *runtimeVaultFixture) bootstrap(mode, token, script string, approved bool) bool {
	v.t.Helper()
	marker := filepath.Join(v.scratch, "complete")
	command := exec.CommandContext(v.ctx, "/bin/sh", script, mode)
	command.Env = []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "HOME=" + v.scratch, "TMPDIR=" + v.scratch,
		"VAULT_BIN=/tools/vault", "VAULT_ADDR=" + v.address, "VAULT_CACERT=" + v.trust, "VAULT_CLIENT_TIMEOUT=3s", "COMPLETION_MARKER=" + marker}
	if approved {
		command.Env = append(command.Env, "RUNTIME_ISOLATION_CUTOVER_APPROVED=yes")
	}
	command.Stdin = strings.NewReader(token + "\n")
	var output runtimeBootstrapOutput
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	for _, fixture := range v.fixtures {
		if strings.Contains(output.String(), fixture.appURL) || strings.Contains(output.String(), fixture.migrationURL) {
			v.t.Fatal("credential entered bootstrap output")
		}
	}
	if strings.Contains(output.String(), token) {
		v.t.Fatal("operator token entered bootstrap output")
	}
	if err == nil {
		if !strings.Contains(output.String(), "BOOTSTRAP_COMPLETE") {
			v.t.Fatal("bootstrap returned success without completion")
		}
		if _, err := os.Stat(marker); err != nil {
			v.t.Fatal("completion marker absent")
		}
	} else if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		v.t.Fatal("failed bootstrap retained a completion marker")
	}
	return err == nil
}

func (v *runtimeVaultFixture) policyState() map[string]string {
	v.t.Helper()
	policies := map[string]string{}
	for _, service := range []string{"accounting", "audit", "identity"} {
		response, status := v.request(http.MethodGet, "sys/policies/acl/"+service+"-read", v.root, nil)
		if status != http.StatusOK {
			v.t.Fatal("fixture policy evidence unavailable")
		}
		data, ok := response["data"].(map[string]any)
		if !ok {
			v.t.Fatal("fixture policy data absent")
		}
		policy, ok := data["policy"].(string)
		if !ok {
			v.t.Fatal("fixture policy text absent")
		}
		policies[service] = policy
	}
	return policies
}

func (v *runtimeVaultFixture) version(path string) int {
	v.t.Helper()
	response, status := v.request(http.MethodGet, "kv/metadata/"+path, v.root, nil)
	if status != http.StatusOK {
		v.t.Fatal("fixture version metadata unavailable")
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		v.t.Fatal("fixture version data absent")
	}
	version, ok := data["current_version"].(float64)
	if !ok || version < 1 || version > 16 || version != float64(int(version)) {
		v.t.Fatal("fixture version outside explicit bound")
	}
	return int(version)
}

func (v *runtimeVaultFixture) fillDummyHistory(path string, fields map[string]any, versions int) {
	v.t.Helper()
	// Fixture-only reset: each independent oracle gets the same clean dummy
	// credential history. This is not a production recovery/cutover operation.
	_, status := v.request(http.MethodDelete, "kv/metadata/"+path, v.root, nil)
	if status != http.StatusNoContent {
		v.t.Fatal("owned dummy record reset failed")
	}
	for range versions {
		v.write("kv/data/"+path, map[string]any{"data": fields})
	}
	if v.version(path) != versions {
		v.t.Fatal("dummy history was not the required boundary")
	}
}

func TestRuntimeVaultIsolation(t *testing.T) {
	address := os.Getenv("TEST_RUNTIME_VAULT_ADDR")
	if address == "" {
		t.Skip("isolated runtime Vault/CSI-policy fixture is not configured")
	}
	if address != "http://127.0.0.1:8200" || os.Getenv("TEST_RUNTIME_VAULT_ADMIN_TOKEN") != "fixture-root-token" {
		t.Fatal("Vault proof accepts only its owned loopback fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("create dummy service-account key")
	}
	v := &runtimeVaultFixture{t: t, ctx: ctx, client: &http.Client{Timeout: 3 * time.Second}, address: address, root: "fixture-root-token", key: key, fixtures: map[string]*runtimeRoleFixture{}, scratch: t.TempDir(), script: runtimeBootstrapScript}
	v.trust = filepath.Join(v.scratch, "fixture-trust.pem")
	// Vault loads VAULT_CACERT while constructing its client even for HTTP.
	// Emit only a valid generated PUBLIC certificate; the existing dummy
	// service-account signing key stays in memory and is never encoded to disk.
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "runtime fixture public CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal("create dummy public trust certificate")
	}
	if err := os.WriteFile(v.trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	v.write("sys/mounts/kv", map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	v.write("sys/audit/file", map[string]any{"type": "file", "options": map[string]string{"file_path": "/scratch/vault-audit.log"}})
	v.write("sys/auth/kubernetes", map[string]string{"type": "kubernetes"})
	v.write("auth/kubernetes/role/postgres-provisioner", map[string]any{"bound_service_account_names": []string{"postgres-provisioner"}, "bound_service_account_namespaces": []string{"kanz-data"}, "audience": "vault", "token_policies": []string{"fixture-unused"}})
	passwords := map[string]any{}
	for _, service := range []string{"accounting", "audit", "identity", "oms", "regulatory", "risk-engine", "venue-binance", "venue-okx"} {
		stem := strings.ReplaceAll(service, "-", "_")
		passwords[stem+"_app"], passwords[stem+"_migrate"] = "dummy-unused-app", "dummy-unused-migration"
		fields := map[string]any{"dsn": "dummy-unused-app", "migrate_dsn": "dummy-unused-migration"}
		if service == "accounting" || service == "audit" || service == "identity" {
			fixture := newRuntimeRoleFixture(t, ctx, service)
			v.fixtures[service] = fixture
			fields["dsn"], fields["migrate_dsn"] = fixture.appURL, fixture.migrationURL
		}
		if service == "identity" {
			signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal("create dummy identity key")
			}
			encoded, err := x509.MarshalECPrivateKey(signingKey)
			if err != nil {
				t.Fatal("encode dummy identity key")
			}
			fields["signing_key_pem"] = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
		}
		v.write("kv/data/kanz/"+service, map[string]any{"data": fields})
	}
	v.write("kv/data/kanz/testnet/postgres", map[string]any{"data": passwords})
	v.write("kv/data/kanz/redis", map[string]any{"data": map[string]string{"password": "dummy-unused-redis"}})
	v.write("kv/data/kanz/api-gateway", map[string]any{"data": map[string]string{"redis_dsn": "dummy-unused-redis", "signing_secret": "dummy-unused-gateway"}})
	if !v.bootstrap("base", v.root, v.script, false) {
		t.Fatal("existing complete bootstrap refused")
	}
	v.configureKubernetes()
	legacyToken := v.login("audit", "kanz-services", "audit", true)
	if v.record(legacyToken, "kanz/audit", 1, true)["migrate_dsn"] != v.fixtures["audit"].migrationURL {
		t.Fatal("affected mixed-record control did not expose the dummy migration credential")
	}
	t.Run("prepare-marker-budget", func(t *testing.T) {
		v.fillDummyHistory("kanz/audit-runtime", map[string]any{"dsn": v.fixtures["audit"].appURL}, 14)
		before := v.policyState()
		if v.bootstrap("runtime-isolation-prepare", v.root, v.script, false) {
			t.Fatal("preparation consumed future activation-marker budget")
		}
		after := v.policyState()
		for service, policy := range before {
			if after[service] != policy {
				t.Fatal("budget refusal mutated a runtime policy")
			}
		}
		for _, endpoint := range []string{"sys/policies/acl/accounting-migrate-read", "auth/kubernetes/role/accounting-migrate", "kv/data/kanz/accounting-runtime", "kv/data/kanz/accounting-migration", "kv/data/kanz/identity-runtime", "kv/data/kanz/identity-migration"} {
			_, status := v.request(http.MethodGet, endpoint, v.root, nil)
			if status != http.StatusNotFound {
				t.Fatal("preparation budget refusal published another record, policy or role")
			}
		}
		if v.version("kanz/audit-runtime") != 14 {
			t.Fatal("budget refusal wrote a credential marker")
		}
		// Version13 plus the preparation marker leaves exactly two writes.
		v.fillDummyHistory("kanz/audit-runtime", map[string]any{"dsn": v.fixtures["audit"].appURL}, 13)
	})
	// A partly copied identity record must neither manufacture a signing key
	// nor publish runtime policy changes. Repair is explicit, not an overwrite.
	v.write("kv/data/kanz/identity-runtime", map[string]any{"data": map[string]string{"dsn": v.fixtures["identity"].appURL}})
	if v.bootstrap("runtime-isolation-prepare", v.root, v.script, false) {
		t.Fatal("partial identity preparation accepted")
	}
	v.record(legacyToken, "kanz/audit", 1, true)
	_, status := v.request(http.MethodDelete, "kv/metadata/kanz/identity-runtime", v.root, nil)
	if status != http.StatusNoContent {
		t.Fatal("explicit dummy partial-record cleanup failed")
	}
	if !v.bootstrap("runtime-isolation-prepare", v.root, v.script, false) {
		t.Fatal("preparation failed")
	}
	if !v.bootstrap("runtime-isolation-prepare", v.root, v.script, false) {
		t.Fatal("idempotent preparation rerun failed")
	}
	if v.bootstrap("runtime-isolation-activate", v.root, v.script, false) {
		t.Fatal("unapproved cutover accepted")
	}
	cleanAudit := v.record(v.root, "kanz/audit-runtime", 0, true)
	t.Run("activation-marker-budget", func(t *testing.T) {
		if v.version("kanz/audit-runtime") != 14 {
			t.Fatal("preparation did not preserve the activation boundary")
		}
		v.write("kv/data/kanz/audit-runtime", map[string]any{"data": cleanAudit})
		before := v.policyState()
		beforeVersions := map[string]int{}
		for _, service := range []string{"accounting", "audit", "identity"} {
			beforeVersions[service] = v.version("kanz/" + service + "-runtime")
		}
		if v.bootstrap("runtime-isolation-activate", v.root, v.script, true) {
			t.Fatal("activation exceeded the marker write budget")
		}
		after := v.policyState()
		for service, policy := range before {
			if after[service] != policy {
				t.Fatal("activation budget refused after publishing a policy")
			}
		}
		for service, version := range beforeVersions {
			if v.version("kanz/"+service+"-runtime") != version {
				t.Fatal("activation budget refusal wrote another service marker")
			}
		}
		if v.version("kanz/audit-runtime") != 15 {
			t.Fatal("activation budget refusal wrote a marker")
		}
		v.fillDummyHistory("kanz/audit-runtime", cleanAudit, 14)
	})
	if !v.bootstrap("runtime-isolation-activate", v.root, v.script, true) {
		t.Fatal("fixture-authorized cutover failed")
	}
	v.record(legacyToken, "kanz/audit", 1, false)
	for _, service := range []string{"accounting", "audit", "identity"} {
		t.Run(service, func(t *testing.T) {
			fixture := v.fixtures[service]
			runtimeToken := v.login(service, "kanz-services", service, true)
			fields := v.record(runtimeToken, "kanz/"+service+"-runtime", 0, true)
			if fields["dsn"] != fixture.appURL || fields["migrate_dsn"] != nil {
				t.Fatal("runtime record acquired migration authority")
			}
			v.record(runtimeToken, "kanz/"+service, 0, false)
			v.record(runtimeToken, "kanz/"+service, 1, false)
			v.record(runtimeToken, "kanz/"+service+"-migration", 0, false)
			for _, other := range []string{"accounting", "audit", "identity"} {
				if other != service {
					v.record(runtimeToken, "kanz/"+other+"-runtime", 0, false)
				}
			}
			v.login(service+"-migrate", "kanz-services", service, false)
			v.login(service, "wrong-namespace", service, false)
			migrationToken := v.login(service+"-migrate", "kanz-services", service+"-migrate", true)
			if v.record(migrationToken, "kanz/"+service+"-migration", 0, true)["migrate_dsn"] != fixture.migrationURL {
				t.Fatal("authorized migration credential unavailable")
			}
			v.record(migrationToken, "kanz/"+service+"-runtime", 0, false)
			v.record(migrationToken, "kanz/"+service, 1, false)
			pool, err := pgxpool.New(ctx, fields["dsn"].(string))
			if err != nil {
				t.Fatal("runtime credential could not be parsed")
			}
			defer pool.Close()
			if err := CheckRuntimePrivileges(ctx, pool, "public"); err != nil {
				t.Fatal("composed runtime identity refused:", err)
			}
		})
	}
	if v.record(v.login("identity", "kanz-services", "identity", true), "kanz/identity-runtime", 0, true)["signing_key_pem"] != v.record(v.root, "kanz/identity", 0, true)["signing_key_pem"] {
		t.Fatal("identity signing material changed during preparation")
	}
	v.write("auth/token/revoke", map[string]string{"token": legacyToken})
	v.record(legacyToken, "kanz/audit-runtime", 0, false)
	v.record(v.login("audit", "kanz-services", "audit", true), "kanz/audit-runtime", 0, true)
	t.Run("boundary-rerun", func(t *testing.T) {
		if v.version("kanz/audit-runtime") != 16 {
			t.Fatal("activation did not finish at its accepted history boundary")
		}
		if !v.bootstrap("base", v.root, v.script, false) {
			t.Fatal("ordinary bootstrap could not preserve boundary isolation")
		}
		if v.version("kanz/audit-runtime") != 16 {
			t.Fatal("ordinary bootstrap changed credential history")
		}
	})
	v.configureKubernetes()
	v.record(v.login("audit", "kanz-services", "audit", true), "kanz/audit", 1, false)
	// Current data looks clean, but an older readable runtime version carries
	// migration authority. Refuse before publishing access, including after a
	// rerun. The disabled-history mutant proves this control is load-bearing.
	fields := v.record(v.root, "kanz/audit-runtime", 0, true)
	fields["credential_isolation_state"] = "v1-prepared"
	// Keep the forbidden-history oracle independent of the history-cap oracle.
	v.fillDummyHistory("kanz/audit-runtime", fields, 1)
	fields["migrate_dsn"] = v.fixtures["audit"].migrationURL
	bad := v.write("kv/data/kanz/audit-runtime", map[string]any{"data": fields})
	version := int(bad["data"].(map[string]any)["version"].(float64))
	delete(fields, "migrate_dsn")
	v.write("kv/data/kanz/audit-runtime", map[string]any{"data": fields})
	if v.bootstrap("runtime-isolation-activate", v.root, v.script, true) {
		t.Fatal("historical migration credential was accepted")
	}
	source, err := os.ReadFile(runtimeBootstrapScript)
	if err != nil {
		t.Fatal(err)
	}
	original := `if field_exists "$history_path" "$forbidden_field" "$history_version"; then`
	if strings.Count(string(source), original) != 1 {
		t.Fatal("history mutation no longer resolves its source boundary")
	}
	mutant := filepath.Join(v.scratch, "disabled-history.sh")
	if err := os.WriteFile(mutant, []byte(strings.Replace(string(source), original, `if false && field_exists "$history_path" "$forbidden_field" "$history_version"; then`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !v.bootstrap("runtime-isolation-activate", v.root, mutant, true) {
		t.Fatal("affected history mutant did not reach the positive control")
	}
	if v.record(v.login("audit", "kanz-services", "audit", true), "kanz/audit-runtime", version, true)["migrate_dsn"] != v.fixtures["audit"].migrationURL {
		t.Fatal("mutation oracle failed to distinguish record history from current projection")
	}
}
