package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A STREAM PROVISIONED FOR A PRODUCER THAT DOES NOT EXIST IS A CLAIM THE ESTATE
// MAKES ABOUT ITSELF (#589).
//
// infra/nats/bootstrap-job.yaml creates the SETTLEMENT stream. infra/nats/
// tenancy.yaml grants the oms workload publish on settlement.instruction.fail
// and names posttrade.BusFailSink by symbol as the thing that will use it.
// Nothing constructs BusFailSink. So the stream is empty on every deployment,
// and an empty stream of settlement fails does not read as "unmonitored" — it
// reads as "no settlement instruction has ever failed".
//
// That is worse than an ordinary dark package, which is why it gets a guard of
// its own rather than sitting under the dark-capability exemption alone. Someone
// checking whether settlement-fail monitoring is live finds a provisioned stream,
// a broker grant and a named producer, and concludes yes from three pieces of
// corroborating evidence, none of which is a process.
//
// # What this binds together
//
// Three artifacts, in both directions:
//
//	darkPackageExempt["services/oms/internal/posttrade"]  the package is unwired
//	infra/nats/*.yaml                                     the manifests say so
//	services/oms/cmd/oms/main.go                          the process says so
//
// While the exemption stands, the manifests must carry the note and the OMS
// composition root must report the posture. When the plane is finally wired the
// exemption goes — and then this guard fails the other way, because manifests
// still saying "no producer exists" about a plane that has one is the same defect
// with the sign flipped. The dead-entry arm on the exemption map catches half of
// that; this catches the half that lives in YAML, which no Go tooling sees.
//
// # Why a guard as well as a comment
//
// The comment is for the person provisioning the estate, who reads the manifest
// and never opens Go. The guard is because a comment is undated evidence: this
// repository has already been bitten by a comment that justified a trade-off
// whose premise had expired. Binding it to the exemption gives it an expiry that
// something enforces.

// settlementDarkPackage is the exemption key this guard is bound to. If the
// post-trade plane is wired, the key disappears from darkPackageExempt and every
// assertion below inverts.
const settlementDarkPackage = "services/oms/internal/posttrade"

// manifestNote is one annotation this guard requires while the plane is dark: a
// file, the marker that must appear in it, and what the marker is load-bearing
// for. The markers are deliberately distinctive strings rather than a phrase that
// could be assembled by accident out of neighbouring prose.
var settlementManifestNotes = []struct {
	path   string
	marker string
	why    string
}{
	{
		path:   filepath.Join("infra", "nats", "bootstrap-job.yaml"),
		marker: "SETTLEMENT HAS NO PRODUCER (#589)",
		why: "the bootstrap job creates the stream. Without the note, the only thing this file " +
			"says about SETTLEMENT is that it matters enough to retain for 168h",
	},
	{
		path:   filepath.Join("infra", "nats", "tenancy.yaml"),
		marker: "GRANTED, AND NOTHING PUBLISHES IT (#589)",
		why: "the grant on settlement.instruction.fail names posttrade.BusFailSink by symbol, " +
			"which reads as a producer that exists and runs",
	},
}

func TestProvisionedSettlementStreamSaysItHasNoProducer(t *testing.T) {
	root := moduleRoot(t)
	_, dark := darkPackageExempt[settlementDarkPackage]

	for _, note := range settlementManifestNotes {
		b, err := os.ReadFile(filepath.Join(root, note.path))
		if err != nil {
			t.Fatalf("read %s: %v", note.path, err)
		}
		body := string(b)
		// Non-vacuity: an empty or moved file would satisfy the "note removed"
		// arm below for the wrong reason.
		if len(strings.TrimSpace(body)) == 0 {
			t.Fatalf("%s is empty — this guard would pass by reading nothing", note.path)
		}
		has := strings.Contains(body, note.marker)

		switch {
		case dark && !has:
			t.Errorf("%s does not carry %q.\n\n%s is still exempt in darkPackageExempt, so no "+
				"process publishes settlement.instruction.fail and the SETTLEMENT stream is empty "+
				"on every deployment. %s — and an empty stream of settlement fails reads as a clean "+
				"book, not as an absent one (#589).", note.path, note.marker, settlementDarkPackage, note.why)
		case !dark && has:
			t.Errorf("%s still carries %q, but %s is no longer exempt in darkPackageExempt.\n\n"+
				"If the post-trade plane has been wired, the manifests are now telling an operator "+
				"that settlement fails are NOT being reported while they are. A stale disclaimer is "+
				"the same defect as a stale claim: remove the note, and drop the "+
				"kanz_oms_settlement_plane_running check below with it.",
				note.path, note.marker, settlementDarkPackage)
		}
	}
}

// AND THE PROCESS MUST SAY IT TOO. The manifests are read at provisioning time by
// whoever is standing the estate up; the startup log and the gauge are what the
// person on call at 3am has. Both manifest notes point at
// kanz_oms_settlement_plane_running, so if the composition root stops calling the
// posture those notes become false.
//
// Asserted off the AST rather than a grep for the identifier: main.go's own
// comment discusses the posture, and a guard that greps raw source matches its
// own explanation — this repository has shipped three guards that passed with the
// checked thing deleted for exactly that reason.
func TestOMSCompositionRootReportsTheSettlementPosture(t *testing.T) {
	if _, dark := darkPackageExempt[settlementDarkPackage]; !dark {
		t.Skipf("%s is wired — the posture is no longer the only signal", settlementDarkPackage)
	}
	const fn = "settlementPlanePosture"
	path := filepath.Join(moduleRoot(t), "services", "oms", "cmd", "oms", "main.go")

	fset := token.NewFileSet()
	// No parser.ParseComments: comments are not in the AST at all here, so a call
	// can only be found by being one.
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var called bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
			called = true
		}
		return true
	})
	if !called {
		t.Fatalf("services/oms/cmd/oms/main.go never calls %s().\n\n"+
			"infra/nats/bootstrap-job.yaml and infra/nats/tenancy.yaml both tell an operator that "+
			"kanz_oms_settlement_plane_running is what distinguishes 'fail detection is not running "+
			"on this deployment' from 'it is running and nothing has failed'. Without this call the "+
			"gauge is never registered, the series is ABSENT rather than zero, and both manifest "+
			"notes are pointing at nothing (#589).", fn)
	}
}
