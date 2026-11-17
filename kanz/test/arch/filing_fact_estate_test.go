package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	"github.com/eighred/kanz/internal/tenantgen"
)

// These are source reach/estate guards, not transaction or broker certificates.
// Actual HTTP→PG→broker tests own atomicity, identity, refusal and readback.
func TestFilingFACTPublishersRemainWired(t *testing.T) {
	files := filingPublisherSources(t)
	if problems := filingPublisherProblems(files); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

const filingServerSource = "services/regulatory/internal/server/server.go"
const filingRecorderSource = "services/regulatory/internal/recording/postgres.go"
const filingMainSource = "services/regulatory/cmd/regulatory/main.go"
const regulatoryPublisherSource = "internal/regulatory/publish/publish.go"
const sustainabilityPublisherSource = "internal/sustainability/publish/publish.go"

func filingPublisherSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := make(map[string]*ast.File)
	for _, path := range []string{filingServerSource, filingRecorderSource, filingMainSource, regulatoryPublisherSource, sustainabilityPublisherSource} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(moduleRoot(t), filepath.FromSlash(path)), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = file
	}
	return files
}

func filingPublisherProblems(files map[string]*ast.File) []string {
	var problems []string
	require := func(path, function, call string) {
		if !filingHasCall(files[path], function, call) {
			problems = append(problems, path+":"+function+" lost required "+call)
		}
	}
	for _, call := range []string{"bus.DialNATS", "bus.NewProducer", "regulatorypublish.New", "recording.New", "server.WithRecorder"} {
		require(filingMainSource, "run", call)
	}
	for _, call := range []string{"p.pool.Begin", "plan.Build", "p.links.AppendChainedTx", "outbox.Enqueue", "tx.Commit", "p.flush"} {
		require(filingRecorderSource, "Record", call)
	}
	require(filingRecorderSource, "Sign", "s.links.AppendChainedTx")
	require(regulatoryPublisherSource, "Publish", "p.producer.Publish")
	require(regulatoryPublisherSource, "Publish", "sustainabilitypublish.Event")
	for _, call := range []string{"s.recorder.Record", "regulatorypublish.Report", "sustainabilitypublish.Build"} {
		require(filingServerSource, "recordFiling", call)
	}
	server := files[filingServerSource]
	routes := map[string]bool{}
	if server != nil {
		ast.Inspect(server, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 || filingSelector(call.Fun) != "s.mux.HandleFunc" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(literal.Value)
			if err != nil || !strings.HasPrefix(path, "POST /v1/filings/") {
				return true
			}
			handler, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok {
				problems = append(problems, "filing route has no named handler")
				return true
			}
			routes[handler.Sel.Name] = true
			return true
		})
	}
	if len(routes) < 5 {
		problems = append(problems, "filing route inventory is incomplete")
	}
	for handler := range routes {
		for _, call := range []string{"s.filingCaller", "readFilingInput", "s.recordFiling"} {
			require(filingServerSource, handler, call)
		}
		computed := false
		for _, name := range []string{"regulatory.FileFRTB", "regulatory.FileFormPF", "regulatory.FileAIFMD", "sustainability.FileTCFD", "sustainability.FileSFDR"} {
			computed = computed || filingHasCall(server, handler, name)
		}
		if !computed {
			problems = append(problems, "filing handler "+handler+" has no existing File* assembler")
		}
	}
	return problems
}

func filingHasCall(file *ast.File, function, selector string) bool {
	if file == nil {
		return false
	}
	found := false
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && filingSelector(call.Fun) == selector {
				found = true
			}
			return true
		})
	}
	return found
}

func filingSelector(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return filingSelector(value.X) + "." + value.Sel.Name
	}
	return ""
}

func TestFilingFACTGuardRejectsDeletedActualPublishAndRecordingEdges(t *testing.T) {
	for _, mutation := range []struct{ path, function, call string }{
		{regulatoryPublisherSource, "Publish", "p.producer.Publish"},
		{filingRecorderSource, "Record", "outbox.Enqueue"},
		{filingRecorderSource, "Record", "p.links.AppendChainedTx"},
		{filingServerSource, "recordFiling", "s.recorder.Record"},
		{filingServerSource, "handleTCFD", "s.filingCaller"},
		{filingMainSource, "run", "server.WithRecorder"},
	} {
		t.Run(mutation.function+"/"+mutation.call, func(t *testing.T) {
			files := filingPublisherSources(t)
			mutated := false
			for _, declaration := range files[mutation.path].Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || fn.Name.Name != mutation.function {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || filingSelector(call.Fun) != mutation.call {
						return true
					}
					if mutation.function == "Publish" {
						// A type-compatible successful no-op, not an undefined-name
						// compile failure. The real broker tests also discriminate it.
						replacement, err := parser.ParseExpr(`func(_ context.Context,_ bus.Event) error {return nil}`)
						if err != nil {
							t.Fatal(err)
						}
						call.Fun = replacement
					} else {
						switch callee := call.Fun.(type) {
						case *ast.SelectorExpr:
							callee.Sel.Name = "Removed"
						case *ast.Ident:
							callee.Name = "Removed"
						}
					}
					mutated = true
					return true
				})
			}
			if !mutated || len(filingPublisherProblems(files)) == 0 {
				t.Fatal("deleted actual source edge escaped guard")
			}
		})
	}
}

func TestFilingFACTEstateRetainsHistoryAndAccountParity(t *testing.T) {
	root := moduleRoot(t)
	streams := provisionedStreams(t)
	archived := derivedArchivedTopics(t, root)
	archiver := archiverDefaultSubjects(t, root)
	audit := auditDefaultSubjects(t)
	for _, fact := range []struct{ subject, stream, topic string }{
		{sustainabilitypublish.ObservedSubject, "RISK", "risk.sustainability"},
		{sustainabilitypublish.AggregatedSubject, "RISK", "risk.sustainability"},
		{regulatorypublish.RecordedSubject, "COMPLIANCE", "compliance.filing"},
	} {
		if !covered(fact.subject, streams[fact.stream]) || archived[fact.topic] != "delete" || !covered(fact.subject, archiver) || !covered(fact.subject, audit) {
			t.Errorf("filing history/stream/archive/audit contract lost for %s", fact.subject)
		}
	}
	accounts := natsAccountUserEntries(t)
	for account, service := range map[string]string{"__system__": "regulatory", "acme": "regulatory-acme"} {
		entry, ok := accounts[account][systemAccountSVID(service)]
		if !ok {
			t.Fatalf("configured filing producer identity missing in %s", account)
		}
		if err := filingPermissionProblem(parsePublishPerm(entry), parseSubscribePerm(entry)); err != "" {
			t.Fatal(account + ": " + err)
		}
	}
	for _, service := range tenantgen.Services {
		if service.Name == "regulatory" {
			t.Fatal("optional filing producer became a default tenant workload")
		}
	}
	service, ok := tenantgen.ServiceByName("regulatory")
	if !ok || service.ComputeSPIFFEID("acme") != systemAccountSVID("regulatory-acme") {
		t.Fatal("tenant filing identity cannot be rendered through the existing explicit generator")
	}
}

func filingPermissionProblem(publish, subscribe publishPerm) string {
	want := []string{sustainabilitypublish.ObservedSubject, sustainabilitypublish.AggregatedSubject, regulatorypublish.RecordedSubject}
	actual := slices.Clone(publish.allow)
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) || len(publish.deny) != 0 || !slices.Equal(subscribe.allow, []string{"_INBOX.>"}) || len(subscribe.deny) != 0 {
		return "filing producer must have exactly three FACT publish subjects and its reply inbox"
	}
	return ""
}

func TestFilingPublisherPermissionGuardRejectsMissingAndAdministrativeGrants(t *testing.T) {
	base := publishPerm{allow: []string{sustainabilitypublish.ObservedSubject, sustainabilitypublish.AggregatedSubject, regulatorypublish.RecordedSubject}}
	inbox := publishPerm{allow: []string{"_INBOX.>"}}
	if problem := filingPermissionProblem(base, inbox); problem != "" {
		t.Fatal(problem)
	}
	for _, mutation := range []string{"missing", "admin", "ack", "domain-wildcard", "read-domain", "no-inbox"} {
		t.Run(mutation, func(t *testing.T) {
			pub := publishPerm{allow: slices.Clone(base.allow)}
			sub := publishPerm{allow: slices.Clone(inbox.allow)}
			switch mutation {
			case "missing":
				pub.allow = pub.allow[:2]
			case "admin":
				pub.allow = append(pub.allow, "$JS.API.>")
			case "ack":
				pub.allow = append(pub.allow, "$JS.ACK.>")
			case "domain-wildcard":
				pub.allow = []string{"risk.>", "compliance.>"}
			case "read-domain":
				sub.allow = append(sub.allow, "risk.>")
			case "no-inbox":
				sub.allow = nil
			}
			if problem := filingPermissionProblem(pub, sub); problem == "" {
				t.Fatalf("%s escaped exact role guard", mutation)
			}
		})
	}
}
