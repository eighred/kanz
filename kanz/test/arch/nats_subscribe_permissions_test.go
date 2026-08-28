package arch

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// EVERY SUBJECT A SERVICE SUBSCRIBES TO IS ONE ITS ACCOUNT ALLOWS (#788).
//
// nats_service_permissions_test.go does this for PUBLISH, deriving each
// service's subject set from the AST. This is the subscribe side, built on the
// same resolveCache so the two directions cannot come to disagree about how a
// subject expression is resolved.
//
// # The two directions are not symmetric, and this is the worse one
//
// A denied publish errors on every attempt: the code sees it, the DLQ sees it,
// somebody is paged. A denied SUBSCRIBE returns no messages — the pod
// authenticates, reports Ready, logs "subscribing to ..." and folds nothing
// forever. That is indistinguishable from a feed nobody is producing on, and
// every control derived from the fold then degrades to its own honest-looking
// refusal.
//
// IT IS NOT HYPOTHETICAL, and the evidence is worded precisely because it is NOT
// reproducible from main. While #787 was being written, the two new compliance
// subscriptions existed with no matching grants and the full arch suite passed
// on that working tree; the missing grants were found by reading tenancy.yaml,
// not by any test. Both landed in one commit, so no such state is in the
// history. What the repository can show is that nothing then in the tree could
// have caught it. The symptom in production would have been the post-trade
// monitor reporting "leverage cannot be verified: cash is unknown" for every
// portfolio on the estate — the same sentence it prints when accounting has
// genuinely never announced.
//
// THE SILENCE IS MEASURED, NOT ASSUMED. internal/bustest's
// TestHowADeniedSubscriptionFails drives the real pkg/bus against a real
// nats-server whose config denies one subject: the connection is ACCEPTED,
// SubscribeBroadcast never returns, and nothing is delivered. So the composition
// roots' fatal-on-subscribe-error path never fires, and the pod stays up folding
// nothing — which is exactly the premise this guard rests on.
//
// # What it resolves, and what it skips
//
// The subject is the SECOND argument of consumer.Subscribe,
// SubscribeBroadcast and SubscribeBroadcastReady, and it is resolved by the
// publish guard's own resolver: a literal, a package-level const across import
// hops, a same-function local, a parameter traced to its call sites, and a
// callee's return tuple folded through every branch.
//
// WHERE IT CANNOT RESOLVE ONE IT SKIPS THAT CALL SITE and logs at -v. An
// unresolved subject is a false negative — accepted, and visible — never a false
// positive that fails the build on correct code. That stance is inherited
// deliberately: a guard that guesses at a subject assembled from configuration
// would be switched off within a week, which is how the publish guard's own
// header argues it.
//
// It also skips anything that does not LOOK like a subject (subjectShape), which
// is what keeps unrelated Subscribe methods out — market-data's Bloomberg, ICE
// and Refinitiv feed adapters all have a Subscribe(ctx, []string) whose second
// argument is a security list, not a subject.
func TestServiceSubscribesOnlyToSubjectsItsTenancyPermissionsAllow(t *testing.T) {
	root := moduleRoot(t)
	perms := serviceSubscribeAllow(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(perms) == 0 {
		t.Fatal("no __system__ kanz-services permissions parsed from tenancy.yaml — has the format changed?")
	}

	rc := newResolveCache(root)

	// A KAFKA TOPIC THAT LOOKS EXACTLY LIKE A NATS SUBJECT.
	//
	// archive.Drain reads DLQSubject through DLQReader, whose own doc comment calls
	// it "the KAFKA side of the drain ... Satisfied by *bus.KafkaClient", and
	// cmd/archiver-drain wires it as Reader: kafka from bus.DialKafka. The comment
	// on DLQSubject in archiver.go says it outright: "dlq.archiver is Kafka". No NATS grant is
	// needed or wanted, and granting one would widen a service's broker
	// permissions to describe a topic that is not on the broker.
	//
	// THE SCANNER CANNOT TELL THEM APART, which is a limit rather than an
	// oversight: bus.KafkaClient.Subscribe(ctx, topic, group, h) has the same
	// arity and argument order as the NATS Consumer.Subscribe, and a Kafka topic
	// here is subject-shaped by construction (topic.For derives it from the
	// subject). Separating them needs the receiver's TYPE, which this AST-only
	// scan does not resolve.
	//
	// THE CLASS IS LIVE, NOT A ONE-OFF: any service that dials NATS and also
	// consumes Kafka through pkg/bus lands here. lake-sink is spared only because
	// dialsNATS returns false for it. A second instance should be exempted the
	// same way; past two or three, the scanner should learn receiver types
	// instead of growing a list.
	exemptSubjects := map[string]string{
		"archiver|dlq.archiver": "a KAFKA topic read through bus.KafkaClient, not a NATS " +
			"subscription — see archive.DLQReader's doc comment and the one on archive.DLQSubject " +
			"(#791 was filed on the opposite reading and closed as invalid)",
	}
	matchedExempt := map[string]bool{}

	var problems []string
	checked, resolved := 0, 0
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, undeployed := notDeployed[svc]; undeployed {
			continue
		}
		if !dialsNATS(t, root, svc) {
			continue
		}
		svid := systemAccountSVID(svc)
		perm, ok := perms[svid]
		if !ok {
			problems = append(problems, svc+": tenancy.yaml has no `permissions` block for "+svid+
				" at all — every subject it subscribes to is unverifiable and, per NATS semantics, "+
				"UNRESTRICTED within __system__")
			continue
		}
		checked++

		subjects := serviceSubscribedSubjects(t, rc, root, svc)
		var found sortedSubjects
		for subj, sites := range subjects {
			found = append(found, subjectSites{subject: subj, sites: sites})
		}
		sort.Sort(found)
		for _, ss := range found {
			resolved++
			if _, ok := exemptSubjects[svc+"|"+ss.subject]; ok {
				matchedExempt[svc+"|"+ss.subject] = true
				continue
			}
			if permDenies(ss.subject, perm) || !subjectAllowed(ss.subject, perm.allow) {
				problems = append(problems, svc+": subscribes to "+strconv.Quote(ss.subject)+" ("+
					strings.Join(ss.sites, ", ")+") but tenancy.yaml's permissions.subscribe for "+svid+
					" does not allow it — the pod will authenticate, report Ready, log that it "+
					"subscribed, and RECEIVE NOTHING on that subject forever, which reads as a quiet "+
					"feed rather than a missing grant")
			}
		}
		t.Logf("%s: %d subscribed subject(s) resolved", svc, len(subjects))
	}

	// NON-VACUOUS ON BOTH COUNTS. Zero services checked means the service list or
	// the NATS-dialing detector broke; zero subjects resolved means the call-site
	// scanner did, and either would report a clean pass over nothing at all.
	if checked == 0 {
		t.Fatal("checked zero services against tenancy.yaml subscribe permissions — the scanner or " +
			"the service list is broken")
	}
	// A FLOOR, NOT A ZERO-CHECK. Zero is not the only broken state: a resolver that
	// quietly stopped reaching one shape would shrink this guard while still
	// finding something, and report PASS. The number is what the tree yields
	// today; a refactor that lowers it is a coverage regression and has to say so
	// out loud rather than pass.
	const resolvedFloor = 16
	if resolved < resolvedFloor {
		t.Fatalf("resolved %d subscribed subject(s) across every service, want at least %d — the "+
			"call-site scanner has lost coverage. Either a subscription moved into a shape the "+
			"resolver cannot reach, or the shape filter is rejecting subjects it used to admit",
			resolved, resolvedFloor)
	}
	// A DEAD EXEMPTION EXCUSES A SUBSCRIPTION NOBODY CHECKED. If the call site
	// goes away — or the resolver stops reaching it, which is the same thing to
	// this guard — the entry must go with it rather than sit here ready to excuse
	// a future subscription that happens to match.
	for key, why := range exemptSubjects {
		if !matchedExempt[key] {
			parts := strings.SplitN(key, "|", 2)
			problems = append(problems, parts[0]+": is exempted for "+strconv.Quote(parts[1])+
				" ("+why+") and no longer subscribes to it. Delete the entry")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d subscription(s) the broker would deny:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// subscribeSurfacesFor returns the SHARED packages to scan on a service's
// behalf — those where the subscription is made inside a package the service
// calls into rather than in its own tree.
//
// internal/platform/halt is the one today. Arm() is how a service joins the kill
// switch (#635): it calls SubscribeBroadcastReady on halt.SubjectModeChanged
// itself, so the subject never appears in the caller's tree, and a service
// missing that grant latches its gate CLOSED and refuses every order while
// reporting Ready.
//
// IT IS DERIVED, NOT LISTED. An earlier draft hand-maintained the six services
// that call halt.Arm — correct on the day it was written, and silently wrong the
// day a seventh joins the kill switch, which is precisely the failure the
// paragraph above describes. callsHaltArm already exists in this package
// (halt_reaches_the_order_path_test.go) and answers the question from the AST,
// so the list is computed from the same source of truth that guard uses.
func subscribeSurfacesFor(t *testing.T, root, svc string) []string {
	t.Helper()
	if callsHaltArm(t, filepath.Join(root, "services", svc, "cmd", svc)) {
		return []string{"internal/platform/halt"}
	}
	return nil
}

func serviceSubscribedSubjects(t *testing.T, rc *resolveCache, root, svc string) map[string][]string {
	t.Helper()
	dirs := []string{filepath.Join(root, "services", svc)}
	for _, extra := range subscribeSurfacesFor(t, root, svc) {
		dirs = append(dirs, filepath.Join(root, filepath.FromSlash(extra)))
	}
	out := map[string][]string{}
	for _, dir := range dirs {
		scanDirForSubscribedSubjects(t, rc, dir, out)
	}
	return out
}

// subscribeMethods are the consumer surfaces that BIND a subject. The subject is
// the second argument of each — ctx first, then the subject — verified against
// the four method declarations on bus.Consumer.
//
// Subscribe                 durable consumer group: one delivery per group.
// SubscribeBroadcast        every replica folds every message.
// SubscribeBroadcastReady   the same, with a signal that the broker accepted it.
// SubscribeReplay           folds a subject's whole retained history, then stays.
//
// SubscribeReplay WAS MISSING from this list and its absence cost the most:
// internal/prediction/registry binds platform.model.registered through it, and
// tenancy.yaml's risk-engine entry argues that grant at length — so the subject
// with the most carefully written justification in the file was the one nothing
// checked.
var subscribeMethods = map[string]bool{
	"Subscribe":               true,
	"SubscribeBroadcast":      true,
	"SubscribeBroadcastReady": true,
	"SubscribeReplay":         true,
}

func scanDirForSubscribedSubjects(t *testing.T, rc *resolveCache, scanDir string, out map[string][]string) {
	t.Helper()
	err := filepath.WalkDir(scanDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		var f *ast.File
		for _, cached := range rc.packageFiles(t, dir) {
			if rc.fset.Position(cached.Pos()).Filename == path {
				f = cached
				break
			}
		}
		if f == nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !subscribeMethods[sel.Sel.Name] {
					return true
				}
				pos := rc.fset.Position(call.Pos())
				rel, rerr := filepath.Rel(scanDir, pos.Filename)
				if rerr != nil {
					rel = pos.Filename
				}
				site := filepath.ToSlash(rel) + ":" + strconv.Itoa(pos.Line)
				for _, subj := range dedupStrings(rc.resolveExpr(t, f, dir, call.Args[1], fn, 6)) {
					if !subscribedSubjectShape(subj) || protoTypeRef.MatchString(subj) {
						continue
					}
					out[subj] = append(out[subj], site)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", scanDir, err)
	}
}

// AND EVERY DECLARED DEFAULT SUBSCRIPTION SET IS GRANTED TOO (#788).
//
// The call-site scan above resolves what it can and skips the rest, which is the
// right stance and leaves most services at zero. Their subscriptions arrive
// through a shape it will not chase: a closure parameter, called from a range
// over a config field, populated from a package-level default with an env
// override — four or five hops, each one a chance to resolve something wrong.
//
// THE SUBJECTS THEMSELVES ARE NOT HIDDEN, though. Every one of those services
// declares its consumption set as a package-level `Default...Subjects` slice in
// its own config package, one hop from a literal, and that slice IS the thing an
// operator changes and the thing the grant has to match.
//
// WHAT IT ACTUALLY COVERS, counted rather than hoped: the ten `Default*Subjects`
// declarations in the tree, minus autopilot's and lineage's (both `notDeployed`,
// skipped before parsing) and minus internal/marketdata/mark's (a shared package
// outside any service tree). An earlier draft of this comment claimed seven
// services; it was wrong, and it was wrong in the direction that matters — the
// arm was checking two subjects from one service while its own documentation
// said otherwise. That is the failure this whole file exists to prevent, so the
// count below is asserted rather than described.
//
// IT IS A DIFFERENT DERIVATION, NOT A DEEPER ONE, and that is the point. It does
// not claim to trace the call graph; it claims that a declared default
// subscription set must be permitted. A service that overrides the default with
// an env var to something wider is outside both arms and is named in the log.
//
// NAMING IS THE DISCRIMINATOR, and it is checked rather than trusted: every
// match is a `Default*Subjects` var, and the survey behind this arm confirmed all
// ten in the tree are consumption sets. A publish-side list named this way would
// be a false positive — it would demand a subscribe grant for a subject the
// service only sends — so if one ever appears, exempt it by name rather than
// widening the grant.
func TestEveryDeclaredDefaultSubscriptionSetIsGranted(t *testing.T) {
	root := moduleRoot(t)
	perms := serviceSubscribeAllow(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(perms) == 0 {
		t.Fatal("no __system__ kanz-services permissions parsed from tenancy.yaml")
	}
	rc := newResolveCache(root)

	var problems []string
	checked := 0
	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, undeployed := notDeployed[svc]; undeployed {
			continue
		}
		if !dialsNATS(t, root, svc) {
			continue
		}
		perm, ok := perms[systemAccountSVID(svc)]
		if !ok {
			continue // the call-site arm already reports a missing permissions block
		}
		sets := declaredDefaultSubjects(t, rc, filepath.Join(root, "services", svc))
		for _, name := range sortedSubjectSetNames(sets) {
			for _, subj := range sets[name] {
				checked++
				if permDenies(subj, perm) || !subjectAllowed(subj, perm.allow) {
					problems = append(problems, svc+": "+name+" declares "+strconv.Quote(subj)+
						" and tenancy.yaml's permissions.subscribe for "+systemAccountSVID(svc)+
						" does not allow it — the default configuration of this service subscribes to "+
						"a subject its own account denies, and a denied subscription receives nothing "+
						"while reporting Ready")
				}
			}
		}
	}

	// A FLOOR, NOT A ZERO-CHECK. `checked == 0` passed while this arm was
	// verifying two subjects from one service and claiming seven — a guard can be
	// non-vacuous and still be checking almost nothing. The number below is what
	// the tree actually yields today; if a refactor drops it, that is a coverage
	// regression and the guard says so instead of quietly shrinking.
	const declaredFloor = 19
	if checked < declaredFloor {
		t.Fatalf("only %d declared default subscription(s) were checked, want at least %d. Either "+
			"a service's Default*Subjects declaration moved out of reach of the resolver, or the "+
			"shape filter is rejecting entries it used to admit — both shrink this guard silently",
			checked, declaredFloor)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d declared default subscription(s) the broker would deny:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// declaredDefaultSubjects returns every package-level `Default...Subjects` slice
// under dir, by name, resolved to literals.
func declaredDefaultSubjects(t *testing.T, rc *resolveCache, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkgDir := filepath.Dir(path)
		var f *ast.File
		for _, cached := range rc.packageFiles(t, pkgDir) {
			if rc.fset.Position(cached.Pos()).Filename == path {
				f = cached
				break
			}
		}
		if f == nil {
			return nil
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !defaultSubjectsName.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range lit.Elts {
						for _, subj := range dedupStrings(rc.resolveExpr(t, f, pkgDir, elt, nil, 6)) {
							if !subscribedSubjectShape(subj) || protoTypeRef.MatchString(subj) {
								continue
							}
							out[name.Name] = append(out[name.Name], subj)
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// defaultSubjectsName matches the convention every service uses to declare what
// it consumes: DefaultSubjects, DefaultFillSubjects, DefaultMarketSubjects, ...
var defaultSubjectsName = regexp.MustCompile(`^Default[A-Za-z0-9]*Subjects$`)

// sortedSubjectSetNames orders the declared sets so the report is deterministic.
// oms_outbox_test.go already owns a sortedKeysOf for map[string]bool; this takes
// a different map type, and one name cannot serve both without a generic nobody
// else in this package needs.
func sortedSubjectSetNames(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// subscribedSubjectShape accepts what a service may actually BIND, which is a
// wider set than what it may publish.
//
// subjectShape (subject_topology_test.go) is the PUBLISH shape: a concrete
// subject, `*` allowed for a variable token, and no `>` — because nothing
// publishes to a wildcard. A SUBSCRIPTION routinely binds one:
// compliance.mandate.changed.>, risk.position.venue.changed.>, and audit's
// whole default set are all trailing-`>` bindings.
//
// USING THE PUBLISH SHAPE HERE SILENTLY DROPPED EVERY ONE OF THEM. The guard
// reported PASS while never checking oms's or compliance's mandate binding,
// optimization's, webhook-ingest's, or any entry in the declared default sets —
// a green guard checking a far weaker property than its name claims. It is
// widened here rather than in subject_topology_test.go because the publish side
// is right to refuse `>`.
func subscribedSubjectShape(subj string) bool {
	if subj == ">" {
		return true
	}
	return subjectShape.MatchString(strings.TrimSuffix(subj, ".>"))
}
