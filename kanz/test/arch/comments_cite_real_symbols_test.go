package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A COMMENT UNDER infra/ MAY NOT NAME A test/arch SYMBOL THAT DOES NOT EXIST (#638).
//
// # What went wrong without it
//
// network-policies.yaml's allow-gateway-to-optimization block carried a
// paragraph making four factual claims about the optimization service and one
// about the guard that watched it. All five were false:
//
//	claim                                            actual
//	no Dockerfile                                    services/optimization/Dockerfile exists
//	no build matrix entry                            build.yml AND release.yml both build it
//	no manifest                                      infra/deploy/optimization-deploy.yaml exists
//	no Deployment                                    it is deployed, metrics annotated on 8094
//	tenantHeaderTrustingUndeployed asserts absence    that identifier existed NOWHERE in the module
//
// The obligation the paragraph deferred had in fact been met — optimization is
// in tenantHeaderTrustingSplitListeners and the split is asserted every run — so
// nothing broke, and nothing noticed.
//
// THE HARM WAS TO THE READER, which is why it is worth a guard rather than a
// silent edit. This is the file where an auditor reads what the platform's
// network authorization IS, and that block explains why a service which
// authenticates NOBODY and stamps the issuer on every order command is safe. A
// reader who trusted it concluded the service was not deployed and stopped; one
// who checked the named guard found nothing and could not tell whether it had
// been renamed, deleted, or never written.
//
// # Why this scope, and why it is not the #610 guard
//
// TestCommentsDoNotCiteRepoFilesByLineNumber (#610) refuses <file>:<line>
// citations in GO comments and requires a symbol instead. Its own doc names what
// it deliberately does not do: "a SYMBOL citation to a symbol that no longer
// exists". That is this one. And its scope is Go and Markdown — YAML under
// infra/ is in neither, which is where the estate's authorization model is
// written.
//
// IT ONLY LOOKS AT COMMENTS THAT MENTION test/arch, and that restraint is the
// design. A YAML comment is full of camelCase that is not a Go symbol —
// podSelector, matchLabels, namespaceSelector, prometheus.io/port — so a scan
// for "identifier-shaped words" would be noise. A sentence that says test/arch
// is making a citation, and that is the construct that rotted here.
func TestInfraCommentsCiteRealArchSymbols(t *testing.T) {
	root := moduleRoot(t)
	declared := declaredGoSymbols(t, root)
	if len(declared) < 500 {
		t.Fatalf("collected only %d Go symbols from the module — the AST scan is broken and this "+
			"guard would report every citation as rotten", len(declared))
	}

	// Identifier-shaped: a camelCase or PascalCase word with an internal capital,
	// long enough not to be an acronym. Bare lowercase words are prose.
	identRe := regexp.MustCompile(`\b[A-Za-z][a-zA-Z0-9]*[A-Z][a-zA-Z0-9]{3,}\b`)

	var problems []string
	scanned := 0
	err := filepath.Walk(filepath.Join(root, "infra"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".yaml") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		scanned++
		for i, line := range strings.Split(readFile(t, path), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "test/arch") {
				continue
			}
			for _, cand := range identRe.FindAllString(trimmed, -1) {
				// ALL-CAPS IS EMPHASIS, NOT AN IDENTIFIER. This repository writes
				// DERIVES, ENGINE, OBSERVER and PROXIED in prose constantly, and every
				// one of them matches an "identifier-shaped word" pattern. A guard that
				// reported those would be turned off within a week, which is the real
				// failure mode for a checker with false positives.
				if cand == strings.ToUpper(cand) {
					continue
				}
				if declared[cand] || infraCitationAllowed[cand] {
					continue
				}
				problems = append(problems, rel+":"+itoaLine(i+1)+": names \""+cand+"\" as a test/arch "+
					"symbol, and no Go declaration in this module has that name. A reader who checks it "+
					"cannot tell whether the guard was renamed, deleted, or never written — which is "+
					"exactly the state tenantHeaderTrustingUndeployed left this file in.")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/: %v", err)
	}
	// NON-VACUITY: infra/ holds dozens of manifests and many cite test/arch.
	if scanned < 20 {
		t.Fatalf("scanned only %d YAML file(s) under infra/ — the walk is broken", scanned)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d comment(s) under infra/ cite a test/arch symbol that does not exist:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// infraCitationAllowed names identifier-shaped words that appear beside
// "test/arch" in an infra comment and are NOT symbol citations — a Kubernetes
// field, a header, a metric. An entry is permission and should be rare; the
// alternative is loosening the identifier pattern, which would make the guard
// stop reporting the thing it exists for.
var infraCitationAllowed = map[string]bool{
	// THE DEAD SYMBOL ITSELF, quoted in network-policies.yaml as the historical
	// record of what the block used to claim. It is the one name in this map that
	// must NEVER exist: if a declaration by that name ever appears, the quotation
	// stops being history and the entry should go. Naming it here rather than
	// rewording the comment keeps the record readable — a reader who greps the
	// old name finds the paragraph explaining why it is gone.
	"tenantHeaderTrustingUndeployed": true,

	"podSelector":        true,
	"namespaceSelector":  true,
	"matchLabels":        true,
	"ipBlock":            true,
	"serviceAccountName": true,
}

// declaredGoSymbols collects every top-level Go declaration name in the module —
// funcs, types, vars and consts — so a citation can be checked against what
// actually exists rather than against a list kept here.
func declaredGoSymbols(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "gen" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil // a file that does not parse is not this guard's business
		}
		for _, d := range file.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Name != nil {
					out[decl.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name != nil {
							out[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							out[n.Name] = true
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for Go symbols: %v", err)
	}
	return out
}

func itoaLine(n int) string {
	if n <= 0 {
		return "?"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
