// Command kanz-tenantgen renders a tenant's compute manifests from the shared
// bases under infra/deploy (see internal/tenantgen) and writes them to
// infra/deploy/tenants/<tenant>/<service>-<tenant>.yaml.
//
// It renders EVERY service internal/tenantgen.Services declares, not just the
// OMS. A tenant given an order path and no consumers publishes its FACTs into a
// NATS account nothing else is a member of — orders in, nothing out, with every
// service reporting healthy (#637). Rendering the set as a unit is what stops a
// tenant being provisioned half-built.
//
// It is the ONLY way a tenant's compute manifest is produced — by hand, via
// provision-tenant.sh's `compute` step, or on regeneration after a base
// changes. Both this tool and test/arch/tenant_compute_test.go's drift guard
// call internal/tenantgen.Render; hand-editing a rendered file instead
// works locally and fails in CI, because the guard re-renders every
// committed tenant manifest from the live base and diffs it byte-for-byte.
//
//	go run ./cmd/kanz-tenantgen -tenant acme
//	go run ./cmd/kanz-tenantgen -tenant acme -service oms
//
// This tool never applies anything to a cluster — it only writes files.
// Committing them to main is what deploys them: infra/deploy/ is
// raw-synced by the ApplicationSet's "workloads" component with
// prune:true/selfHeal:true, so an out-of-band kubectl apply would come up
// and then be pruned within minutes because it does not exist in git.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eighred/kanz/internal/tenantgen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-tenantgen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("kanz-tenantgen", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant id (lowercase, dns-safe) — required")
	service := fs.String("service", "", "render only this declared service (default: all of them)")
	root := fs.String("root", ".", "module root the base paths are resolved against")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenant == "" {
		return fmt.Errorf("-tenant is required")
	}

	services := tenantgen.Services
	if *service != "" {
		svc, ok := tenantgen.ServiceByName(*service)
		if !ok {
			return fmt.Errorf("no declared per-tenant service %q — declared: %s "+
				"(adding one is a platform decision; see internal/tenantgen/services.go)",
				*service, strings.Join(declaredNames(), ", "))
		}
		services = []tenantgen.Service{svc}
	}

	for _, svc := range services {
		baseBytes, err := tenantgen.ReadBases(*root, svc)
		if err != nil {
			return err
		}
		rendered, err := tenantgen.Render(baseBytes, svc, *tenant)
		if err != nil {
			return err
		}
		outPath := filepath.Join(*root, filepath.FromSlash(svc.ManifestPath(*tenant)))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(outPath), err)
		}
		if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", outPath, err)
		}
		fmt.Printf("wrote %s\n", outPath)
		fmt.Printf("  NATS: tenancy.yaml's %q account must admit %s, or the pod authenticates and maps to NO account (SEC-M3)\n",
			*tenant, svc.ComputeSPIFFEID(*tenant))
		// WHAT THIS TENANT DOES NOT GET. A dropped variable is a capability the
		// base deployment has and this render deliberately withholds, so it is
		// printed beside the file rather than left to whoever diffs the two.
		for _, d := range svc.DropEnv {
			fmt.Printf("  WITHHELD: %s - %s\n", d.Name, d.Why)
		}
		if svc.KafkaProducer {
			fmt.Printf("  KAFKA: that same principal needs the %q. PREFIXED ACL — tenantctl.sh's onboard grants it from COMPUTE_KAFKA_SAS\n", *tenant)
		}
	}
	return nil
}

func declaredNames() []string {
	var out []string
	for _, s := range tenantgen.Services {
		out = append(out, s.Name)
	}
	return out
}
