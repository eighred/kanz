package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// A HOLDING'S MARK IS USABLE ONLY IF EVERY FIELD OF IT IS PRESENT (#806).
//
// # Why this guard exists, and why it is driven by the proto
//
// This is the THIRD time the same hole has been repaired, each time one field
// short of the last:
//
//   - #760 found that heldPositions dropped a position whose MarketValue was
//     nil. The repair caught nil.
//   - The same issue then found a Money carrying a currency and NO AMOUNT, which
//     reached absRatFromMoney identically. The repair added the amount.
//   - #806 found a Money carrying an AMOUNT AND NO CURRENCY CODE — a number with
//     no unit. CurrencyRule read the code off that field and SKIPPED what it
//     could not read, so a currency restriction, which is a mandate term, was
//     silently satisfied by the one holding it was written to refuse. The same
//     field is bucketKey's DIMENSION_CURRENCY, so the holding also became an
//     anonymous "" bucket and a per-currency weight was measured against a
//     denominator nobody can name.
//
// Three repairs, one pattern: somebody enumerated the ways a mark can be absent
// and missed one. A guard that enumerated them too would be a fourth copy of the
// same fallible list.
//
// So this one asks the PROTO. common.v1.Money is the schema of a mark; every
// field it declares is part of what "the platform priced this holding" means, and
// unmarkedReason must have an opinion about each. If Money ever grows a third
// field, this fails until somebody decides whether an absent one is a usable
// mark — which is the decision that was skipped three times.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraphs
// above name GetAmount and GetCurrencyCode; the check below runs over parsed
// selector expressions inside one named function's BODY, so this text cannot
// satisfy it.
const complianceRulesFile = "internal/compliance/rules.go"

// unmarkedReasonFunc is the one place that decides whether a mark is usable.
const unmarkedReasonFunc = "unmarkedReason"

// moneyFieldExempt names a Money field an unusable-mark check may ignore, and
// the issue that retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An entry here is a decision that a holding whose
// mark is missing that field is still safe to evaluate a mandate against — which
// has to be argued in writing before it is true in code. That argument is
// exactly what nobody made for currency_code, and the cost was a mandate term
// reporting compliant over the holding it existed to refuse.
var moneyFieldExempt = map[string]string{}

func TestAUsableMarkCoversEveryMoneyField(t *testing.T) {
	// The fields of a mark, from the schema rather than from a list somebody
	// maintains here.
	fields := (&commonpb.Money{}).ProtoReflect().Descriptor().Fields()
	want := map[string]string{} // getter name -> proto field name
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if _, ok := moneyFieldExempt[name]; ok {
			continue
		}
		want[getterName(name)] = name
	}
	if len(want) == 0 {
		t.Fatal("common.v1.Money declares no unexempted fields — this guard is asserting nothing")
	}

	path := filepath.Join(moduleRoot(t), filepath.FromSlash(complianceRulesFile))
	fset := token.NewFileSet()
	// Mode 0: comments are not attached, so this cannot match its own prose.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", complianceRulesFile, err)
	}

	var body *ast.BlockStmt
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == unmarkedReasonFunc && fn.Body != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("%s has no func %s — the single decision point for whether a mark is usable has "+
			"been renamed or removed. If it moved, point this guard at it; if it was inlined into "+
			"each rule, that is the shape #760 and #806 both came from",
			complianceRulesFile, unmarkedReasonFunc)
	}

	called := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			called[sel.Sel.Name] = true
		}
		return true
	})

	var missing []string
	for getter, field := range want {
		if !called[getter] {
			missing = append(missing, field+" (no "+getter+" call)")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s does not test %d of common.v1.Money's fields: %s.\n"+
			"A mark missing one of them is not a mark the platform can act on, and a rule that "+
			"reads that field will skip what it cannot read — which is how a currency "+
			"restriction came to be satisfied by a holding with no currency (#806), after the "+
			"same hole had already been repaired twice one field at a time (#760). Either test "+
			"the field in %s, or add it to moneyFieldExempt with the issue that argues why a "+
			"mandate may be evaluated over a mark without it",
			unmarkedReasonFunc, len(missing), strings.Join(missing, ", "), unmarkedReasonFunc)
	}
}

// getterName is protoc-gen-go's field-to-getter mapping: snake_case becomes
// GetCamelCase.
func getterName(field string) string {
	var b strings.Builder
	b.WriteString("Get")
	for _, part := range strings.Split(field, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}
