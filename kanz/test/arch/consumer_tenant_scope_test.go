package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SERVICE THAT FOLDS EVENTS INTO A TENANT-SCOPED STORE MUST CHECK THE
// ENVELOPE'S TENANT (#223).
//
// Consumers read env.GetTenantId() for routing decisions and then WRITE through
// an RLS pool pinned to their OWN configured tenant. The OMS is the clearest
// case: service.go resolved the venue account from the envelope's tenant while
// postgres.go inserted `VALUES (current_setting('app.tenant_id'), …)` — the GUC
// pg.NewTenantPool pins to cfg.Tenant. Nothing compared them, so tenant acme's
// order was admitted from an acme envelope and stored as __system__. accounting,
// alternatives and wealth folded the same way.
//
// The platform already had this guard twice and did not generalise it — tv-sync's
// projection and internal/topic.For — which is exactly the shape of defect a
// guard exists to stop: the correct pattern present, and not applied.
//
// SCOPE: a service is in scope when it both (a) constructs a tenant-pinned pool
// via pg.NewTenantPool, and (b) defines at least one bus.EventHandler. (a) alone
// is a service that reads but does not fold; (b) alone is a service with no
// tenant-scoped store to corrupt.
// Keyed "service/relative-path" — per FILE, matching the granularity the scan
// reports. A service-wide key would have exempted accounting's ledger fold along
// with its FX fold, which is the opposite of what either needs.
var consumerTenantScopeExempt = map[string]string{
	"accounting/internal/fxfeed/fxfeed.go": "LiveFX folds UNIVERSAL MARKET DATA, not " +
		"tenant-scoped state: FX rates arriving on market.* are the same fact for every " +
		"tenant, which is why price_observations deliberately carries no RLS. Refusing a " +
		"rate because the envelope names another tenant would degrade the NAV conversion " +
		"cache for no isolation benefit — there is nothing private in a EURUSD mid. The " +
		"cache is in-memory and keyed by currency, not by tenant, so there is no " +
		"tenant-scoped store here to corrupt.",

	"tv-sync/internal/projection/projection.go": "tv-sync predates the shared helper and implements the same rule inline: " +
		"projection.Handle compares env tenant against p.tenant and skips a foreign one " +
		"Its semantics differ " +
		"deliberately — it SKIPS rather than refuses, because its subscription legitimately " +
		"carries other tenants' facts. Migrating it onto bus.RequireTenantScope would change " +
		"that behaviour and belongs with #97, not here.",
}

// The archiver deliberately has NO entry above. It enforces the same rule more
// strictly one layer down — internal/topic.For refuses when env.TenantId is not
// the archiver's tenant, so a cross-tenant event cannot reach a Kafka topic at
// all — but it writes to Kafka rather than a tenant-pinned pool, so the scope
// filter already excludes it and an exemption would be dead permission.
//
// Recorded because the first draft of this guard DID exempt it, and the
// dead-entry arm below rejected the entry. That is the arm working: an exemption
// granted on a plausible-sounding reason, for a service that never needed one.

var (
	// The EventHandler shape, which is uniform across this module.
	eventHandlerSig = regexp.MustCompile(`env \*envelopepb\.Envelope, payload \[\]byte\) error`)
	tenantPoolCall  = regexp.MustCompile(`pg\.NewTenantPool\(`)
	scopeCall       = regexp.MustCompile(`RequireTenantScope\(`)
)

// scanService walks one service directory and reports what it contains.
//
// PER-FILE, NOT PER-SERVICE, and that distinction is the guard. The first draft
// asked only "does this service call RequireTenantScope anywhere", and a mutation
// exposed it as nearly vacuous: deleting the check from the OMS's command handler
// still passed, because the OMS's position projector calls it in a different
// file. A service with five handlers and one check would have satisfied it.
//
// unguarded is therefore every FILE that defines a bus.EventHandler and does not
// itself perform the check.
func scanService(t *testing.T, dir string) (unguarded []string, hasPool, hasHandler bool) {
	t.Helper()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		body := string(b)
		if tenantPoolCall.MatchString(body) {
			hasPool = true
		}
		if !eventHandlerSig.MatchString(body) {
			return nil
		}
		hasHandler = true
		if !scopeCall.MatchString(body) {
			rel, _ := filepath.Rel(dir, path)
			unguarded = append(unguarded, filepath.ToSlash(rel))
		}
		return nil
	})
	return
}

func TestEveryTenantScopedConsumerChecksTheEnvelopeTenant(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "services")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}

	var missing []string
	inScope := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		unguarded, hasPool, hasHandler := scanService(t, filepath.Join(root, e.Name()))
		if !hasHandler || !hasPool {
			continue
		}
		inScope++
		for _, f := range unguarded {
			key := e.Name() + "/" + f
			if _, ok := consumerTenantScopeExempt[key]; ok {
				continue
			}
			missing = append(missing, key)
		}
	}

	// NON-VACUITY. Several services demonstrably both fold events and pin a pool.
	// A scan that finds none means the shapes this guard keys on have moved, and
	// it would pass no matter what the consumers do.
	if inScope < 3 {
		t.Fatalf("only %d service(s) matched \"folds events AND pins a tenant pool\" — expected "+
			"at least 3. The guard is not finding the consumers, so it is asserting nothing", inScope)
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these files define a bus.EventHandler in a service that folds into a tenant-pinned "+
			"store, and do not compare the envelope's tenant to their own:\n  %s\n\n"+
			"The handler reads env.GetTenantId() for routing and the store writes "+
			"current_setting('app.tenant_id') — one tenant's event folded into another's book, "+
			"silently (#223).\n\n"+
			"Call bus.RequireTenantScope(env.GetTenantId(), <this service's tenant>) at the "+
			"dispatch point and return its error. It is deliberately INERT while the service "+
			"serves __system__, so adopting it does not change today's shared deployment — it is "+
			"what makes per-tenant compute (#97) safe.", strings.Join(missing, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a service that now checks, or no longer
	// qualifies, is stale permission.
	var dead []string
	for key := range consumerTenantScopeExempt {
		svc, rel, ok := strings.Cut(key, "/")
		if !ok {
			dead = append(dead, key+" (malformed key: want service/relative-path)")
			continue
		}
		if _, err := os.Stat(filepath.Join(root, svc, filepath.FromSlash(rel))); err != nil {
			dead = append(dead, key+" (no such file)")
			continue
		}
		unguarded, _, _ := scanService(t, filepath.Join(root, svc))
		still := false
		for _, f := range unguarded {
			if f == rel {
				still = true
			}
		}
		if !still {
			dead = append(dead, key+" (no longer an unguarded handler — exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("consumerTenantScopeExempt has %d stale entr(y/ies):\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
