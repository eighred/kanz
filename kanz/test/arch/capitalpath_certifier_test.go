package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #71's certifier is evidence over the governed path, never a second
// execution path. These are architectural boundaries: weakening one would make
// a green physical test prove a route institutional clients cannot take.
func TestCapitalpathCertifierCannotBecomeAVenueClient(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "test", "live", "capitalpath")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	files := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		files++
		source.Write(raw)
	}
	if files < 7 {
		t.Fatalf("read only %d production Go files from the certifier; guard would be vacuous", files)
	}
	text := source.String()
	for _, required := range []string{
		`"/v1/orders"`, "SubscribeReplay", "pg.NewTenantPool", `secret.Read("CAPITALPATH_GATEWAY_TOKEN")`,
		"subjectPortfolioCash", "CausationId", "orderid.Mint",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("capitalpath certifier no longer contains required governed-path seam %q", required)
		}
	}
	for _, forbidden := range []string{
		"services/venue-", "internal/venueadapter", "internal/execution", "venue.v1",
		"OKX_API_KEY", "OKX_SECRET_KEY", "BINANCE_API_KEY", "BINANCE_SECRET_KEY",
		"InsecureSkipVerify", "EVENT_CLASS_COMMAND", "order.order.submit",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("capitalpath certifier contains forbidden direct-capital-path capability %q", forbidden)
		}
	}
}

func TestCapitalpathCertifierShipsAndRunsWithBoundedCredentials(t *testing.T) {
	root := moduleRoot(t)
	repo := filepath.Dir(root)
	mustContain := func(path string, fragments ...string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, fragment := range fragments {
			if !strings.Contains(text, fragment) {
				t.Errorf("%s is missing %q", path, fragment)
			}
		}
		return text
	}

	dockerfile := mustContain(filepath.Join(root, "test", "live", "capitalpath", "Dockerfile"),
		"distroless-static:nonroot", "CGO_ENABLED=0", "./test/live/capitalpath")
	if strings.Contains(dockerfile, "API_KEY") || strings.Contains(dockerfile, "SECRET_KEY") {
		t.Fatal("capitalpath image bakes an exchange credential name into its build surface")
	}
	for _, workflow := range []string{"build.yml", "release.yml"} {
		mustContain(filepath.Join(repo, ".github", "workflows", workflow),
			"service: kanz-capitalpath", "kanz/test/live/capitalpath/Dockerfile")
	}
	runner := mustContain(filepath.Join(root, "infra", "dr", "run-capitalpath-certification.sh"),
		"@sha256:", "backoffLimit: 0", "CAPITALPATH_GATEWAY_TOKEN_FILE", "CAPITALPATH_LEDGER_DSN_FILE",
		"readOnlyRootFilesystem: true", "CAPITALPATH_DR_ATTESTATION_FILE", "CAPITALPATH_GATEWAY_CIDR", "CAPITALPATH_LEDGER_CIDR")
	for _, forbidden := range []string{"kubectl create secret", "OKX_API_KEY", "BINANCE_API_KEY", "0.0.0.0/0 } }"} {
		if strings.Contains(runner, forbidden) {
			t.Errorf("capitalpath runner contains forbidden credential or open-egress shape %q", forbidden)
		}
	}
}

func TestCapitalpathNATSIdentityIsBusinessReadOnly(t *testing.T) {
	root := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, `user: "spiffe://kanz.internal/ns/tenant-acme/sa/kanz-capitalpath"`)
	if start < 0 {
		t.Fatal("capitalpath SVID is not mapped into the tenant NATS account")
	}
	end := strings.Index(text[start:], "# The tenant's workloads present this SVID")
	if end < 0 {
		t.Fatal("could not bound capitalpath NATS permission block")
	}
	block := text[start : start+end]
	wantPublish := []string{
		"dlq.order.order.accepted", "dlq.order.order.rejected",
		"dlq.order.order.routed", "dlq.order.order.partially_filled",
		"dlq.order.order.filled", "dlq.accounting.balance.portfolio",
		"$JS.API.>", "$JS.ACK.>",
	}
	gotPublish := parsePublishPerm(block).allow
	if missing, extra := setDifference(wantPublish, gotPublish), setDifference(gotPublish, wantPublish); len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("capitalpath NATS identity has the wrong read-consumer/DLQ publish grant: missing=%v extra=%v", missing, extra)
	}
	for _, subject := range []string{"order.order.accepted", "order.order.routed", "order.order.filled", "accounting.balance.portfolio"} {
		if !strings.Contains(block, subject) {
			t.Errorf("capitalpath NATS identity cannot observe %s", subject)
		}
	}
	if strings.Contains(block, "order.order.submit") || strings.Contains(block, "order.order.cancel") {
		t.Fatal("capitalpath NATS identity can issue an order command")
	}
}
