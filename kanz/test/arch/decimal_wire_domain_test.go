package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A BUS CONSUMER THAT CONVERTS A DECIMAL OFF THE WIRE MUST BOUND ITS DOMAIN (#95).
//
// dec.FromProto materialises 10^abs(exponent) with no bound, and Decimal.exponent
// is an UNVALIDATED wire field: a FACT carrying {Coefficient:1, Exponent:2000000000}
// does not produce a wrong number, it never returns. The failure is therefore not a
// bad value in a book — it is a consumer that stops acking, so the subscription
// stalls behind one message and everything downstream of it goes quiet while looking
// healthy. That is the worst shape of failure this platform has.
//
// Two ingresses were bounded one at a time before this guard existed — the market-data
// fold, then the pre-trade gate — and each time the NEXT unguarded consumer was found
// by reading code rather than by anything failing. Nine more were still unbounded when
// #95 was opened, including the accounting ledger (cash AND fills), the webhook
// position cache a CLOSE is sized from, the OMS execution book, the OMS command
// handler and the post-trade compliance monitor.
//
// So the rule is not "call the checked variant here". A file that decodes a bus payload
// must perform a domain check if EITHER:
//
//	it converts a Decimal itself           — dec.FromProto in the same file, or
//	it hands the decoded message onward    — returns *…pb.…, so the Decimals leave
//	                                         undecoded and the conversion is elsewhere
//
// The second arm is the one that matters. services/oms/internal/position/projector.go
// decodes a fill and returns *orderpb.Fill; book.go and postgres.go convert its price
// and quantity two files away. Neither file breaks a rule about itself, which is
// exactly how the OMS execution book was still unguarded after two other ingresses had
// been fixed.
//
// WHAT THIS GUARD DOES NOT PROVE, stated plainly because a guard trusted past its reach
// is worse than none:
//
//   - It is an AST PAIRING check. It confirms a domain check exists in a file that needs
//     one, not that it sits on the path the conversion is on. A file could satisfy it and
//     still convert unchecked down another branch.
//   - It does not see a message handed on as an ARGUMENT (applier.Apply(ctx, env, &p))
//     rather than returned. Those boundaries — risk ingest, market-data ingest,
//     livequote, the compliance monitor, the OMS command handler — were checked by hand
//     for #95 and carry dec.InDomainDeep, but a NEW one would not be caught here.
//   - services/lake-sink decodes into a dynamicpb message and re-marshals it to JSON
//     without ever converting a Decimal. It is not exempted, it is genuinely outside the
//     rule: refusing there would drop archival data to prevent an expansion that path
//     never performs.
//
// That residue is why the refusal belongs in dec.InDomainDeep — which walks the whole
// decoded message rather than the fields a given decoder happens to read — and why the
// dec package's own tests, not this guard, are what prove the bound holds.

// decodesBusPayload matches proto.Unmarshal(payload, …) — the platform's uniform
// spelling for "this is a FACT off the bus", since bus.EventHandler's third
// parameter is named payload everywhere.
const busPayloadParam = "payload"

// domainCheckers are the calls that bound a Decimal's exponent. Either satisfies
// the rule: InDomainDeep refuses a whole message, FromProtoChecked refuses one
// conversion, and both read the same bound out of internal/dec.
var domainCheckers = map[string]bool{
	"InDomainDeep":     true,
	"FromProtoChecked": true,
	"InDomain":         true,
}

// pendingWireDomainChecks are payload decoders that still convert with the
// unchecked dec.FromProto.
//
// EMPTY, and that is the point: it was the #95 worklist and the work is done. The
// stale-exemption arm below FAILS the build when an entry no longer matches, so
// this map cannot quietly accumulate excuses. A new entry needs a written reason
// naming the issue that retires it, and should be temporary.
var pendingWireDomainChecks = map[string]string{}

func TestBusPayloadDecodersBoundTheDecimalDomain(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	scanned, decoders, guarded := 0, 0, 0
	seenPending := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Generated SDKs and fixtures are not hand-written consumers.
			if n := info.Name(); n == "testdata" || n == "vendor" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}

		var decodesPayload, convertsUnchecked, checksDomain, handsOutProto bool
		var firstUnchecked token.Position
		var handOff string

		// THE HAND-OFF CASE, which an in-file rule cannot see.
		//
		// services/oms/internal/position/projector.go decodes a fill FACT and
		// RETURNS *orderpb.Fill; book.go and postgres.go then convert its price and
		// quantity with the unbounded dec.FromProto. Neither file breaks a rule about
		// itself — the decode and the conversion are two files apart — and that is
		// exactly how the OMS execution book was still unguarded after two other
		// ingresses had been fixed.
		//
		// So a function that unmarshals a payload and hands the message out still
		// typed is treated as needing the check: it is the last point at which the
		// whole message is in one place, and it has just declared that the Decimals
		// leave undecoded.
		for _, d := range f.Decls {
			fn, isFn := d.(*ast.FuncDecl)
			if !isFn || fn.Body == nil || fn.Type.Results == nil {
				continue
			}
			unmarshalsHere := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Unmarshal" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "proto" {
					return true
				}
				if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == busPayloadParam {
					unmarshalsHere = true
				}
				return true
			})
			if !unmarshalsHere {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if name, ok := protoResultType(res.Type); ok {
					handsOutProto = true
					handOff = fmt.Sprintf("%s returns %s", fn.Name.Name, name)
				}
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "proto" && sel.Sel.Name == "Unmarshal" && len(call.Args) > 0:
				if id, isIdent := call.Args[0].(*ast.Ident); isIdent && id.Name == busPayloadParam {
					decodesPayload = true
				}
			case pkg.Name == "dec" && sel.Sel.Name == "FromProto":
				if !convertsUnchecked {
					firstUnchecked = fset.Position(call.Pos())
				}
				convertsUnchecked = true
			case pkg.Name == "dec" && domainCheckers[sel.Sel.Name]:
				checksDomain = true
			}
			return true
		})

		if !decodesPayload {
			return nil
		}
		decoders++
		if checksDomain {
			guarded++
		}
		if checksDomain || (!convertsUnchecked && !handsOutProto) {
			return nil
		}

		rel := filepath.ToSlash(mustRel(root, path))
		if _, pending := pendingWireDomainChecks[rel]; pending {
			seenPending[rel] = true
			return nil
		}
		if convertsUnchecked {
			offenders = append(offenders, fmt.Sprintf("%s:%d converts a Decimal with dec.FromProto",
				rel, firstUnchecked.Line))
		} else {
			offenders = append(offenders, fmt.Sprintf("%s: %s — the Decimals leave this file undecoded",
				rel, handOff))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUITY, in three parts. This guard is a conjunction, so it fails open in
	// three different ways, and each has to be closed separately or a broken walk
	// reads as a clean repository.
	if scanned == 0 {
		t.Fatal("scanned zero Go files — the walk is broken")
	}
	if decoders == 0 {
		t.Fatal("found no files decoding a bus payload — the proto.Unmarshal(payload, …) " +
			"detection has stopped matching, so every consumer would pass this guard unexamined")
	}
	if guarded == 0 {
		t.Fatal("no payload decoder performs a domain check — either every consumer regressed " +
			"at once, or the dec.InDomainDeep/FromProtoChecked detection has stopped matching " +
			"and this guard can now only ever pass")
	}

	// A declared exception whose file no longer decodes a payload with an unchecked
	// conversion is DEAD. Removing it is how the follow-up reports progress; leaving
	// it lets a future regression hide behind a stale entry.
	for file := range pendingWireDomainChecks {
		if !seenPending[file] {
			offenders = append(offenders, file+
				": declared in pendingWireDomainChecks but no longer converts an unchecked "+
				"dec.FromProto off a payload — stale exemption, remove it")
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("bus payload decoders convert a Decimal with the UNBOUNDED dec.FromProto:\n  %s\n\n"+
			"Decimal.exponent is an unvalidated wire field and dec.FromProto materialises "+
			"10^abs(exponent). A FACT carrying {1, 2000000000} does not fold a wrong number — it "+
			"never returns, the consumer stops acking, and the subscription stalls behind that one "+
			"message while the service still reports healthy.\n\n"+
			"Call dec.InDomainDeep on the decoded message right after proto.Unmarshal and refuse "+
			"it the way that decoder already refuses a malformed payload — it walks EVERY Decimal "+
			"in the message, so a field added to the schema later is covered without anyone "+
			"returning here. Where a single conversion has its own error path, dec.FromProtoChecked "+
			"is enough. Never substitute zero for a refused value: a zero price or quantity reads "+
			"as flat and is dropped from downstream checks.",
			strings.Join(offenders, "\n  "))
	}
}

// protoResultType reports whether a result type is a pointer to a generated
// protobuf message, and names it. The repository imports every generated package
// under a "…pb" alias (orderpb, domainpb, accountingpb), which is what makes this
// recognisable without type-checking the whole module.
func protoResultType(e ast.Expr) (string, bool) {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !strings.HasSuffix(pkg.Name, "pb") {
		return "", false
	}
	return "*" + pkg.Name + "." + sel.Sel.Name, true
}
