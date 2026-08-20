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

// SEC-M3f: the domain-subject guard above (SEC-M3e,
// nats_service_permissions_test.go) derives each service's PUBLISH set from
// code and fails the build if tenancy.yaml doesn't allow it — but it checks
// DOMAIN subjects only (`market.>`, `order.order.submit`, ...). It has no
// opinion on `$JS.API.>`, `$JS.ACK.>`, or `_INBOX.>`: those are not domain
// subjects, they are the NATS/JetStream MACHINERY every publish and every
// JetStream consumer transaction rides on, and a service missing one of them
// sails through the domain-subject guard clean.
//
// That gap is worse than a missing domain grant, not merely equivalent to it.
// A denied domain publish fails LOUDLY — SEC-M3e's own header: "the service
// will authenticate fine and then be DENIED at publish time ... which looks
// like a broken feature, not a missing grant". A denied `$JS.ACK.>` publish
// does not fail loudly at all: pkg/bus/nats.go's Subscribe calls `m.Ack()`,
// and on failure only LOGS it (logAckFailure) — the error never reaches the
// caller. So the failure mode is: the consumer processes the message
// successfully, the ack is silently denied, the broker redelivers the SAME
// message once AckWait expires, the handler reprocesses it, and this repeats
// forever — while every health check the service exports keeps reporting
// green. It reads as load, not as a permissions error, and nothing points at
// tenancy.yaml.
//
// This guard derives, from evidence two existing scanners already gather —
// not a third AST scanner — which JetStream ROLE each __system__ user plays:
//
//   - CONSUMES: the service calls bus.NewConsumer, and therefore Subscribe /
//     SubscribeBroadcast / SubscribeBroadcastReady on the *Consumer it
//     returns (busConsumerUnits below reuses bus_dlq_test.go's
//     busConsumerCalls — the exact scanner TestEveryBusConsumerWiresADLQ
//     already trusts for this same call site). Per pkg/bus/nats.go, every one
//     of those calls resolves the stream and creates/binds the consumer under
//     `$JS.API.>`, acks under the separate `$JS.ACK.>` space, and needs
//     `_INBOX.>` for the request/reply those calls make. Requires publish
//     `$JS.API.>` AND `$JS.ACK.>`, subscribe `_INBOX.>`.
//   - PUBLISHES: the service (or operator CLI) constructs at least one
//     bus.Event (servicePublishedSubjects / publishedSubjectsIn below — the
//     exact resolver SEC-M3e already trusts to find a service's domain
//     publishes). jetstream.PublishMsg needs `_INBOX.>` to receive its own
//     publish ack. It does NOT need `$JS.API.>` — established by a real-broker
//     run whose report was deleted with the plan tree on 2026-07-29, so treat it
//     as dated evidence and re-run it before relying on it: a
//     publish-only tool granted `$JS.API.>` anyway would hold the full
//     JetStream admin surface (stream/consumer create, delete, purge), a far
//     bigger privilege than PublishMsg needs or least-privilege calls for.
//   - A user may be both; the two requirements simply union (a consumer that
//     also publishes still only needs `_INBOX.>` once).
//
// SCOPE, PREVIOUSLY DISHONEST: archiver calls `a.cfg.NATS.Subscribe` — the
// *NATSClient method — directly, never `bus.NewConsumer`, so it never
// appeared in busConsumerUnits and this guard used to derive NO requirement
// for it. That gap is exactly what let tenancy.yaml's archiver entry ship as
// `publish: { deny: [">"] }` (SEC-M3e) without this guard ever objecting:
// pkg/bus/nats.go's Subscribe method runs the identical JetStream
// consume-and-Ack machinery — `CreateOrUpdateConsumer` under `$JS.API.>`,
// `Msg.Ack()` under `$JS.ACK.>` — no matter whether the caller reached it via
// `bus.NewConsumer` or by holding a `*bus.NATSClient` (or an interface/field
// backed by one, as `Config.NATS` is in services/archiver/internal/archive)
// and calling `Subscribe` on it directly. A denied `$JS.ACK.>` would have
// meant archiver acknowledges nothing on a real broker, ever, and every
// message it archives redelivers forever — the exact silent failure mode
// this file's header describes, just reached through a call site the guard
// could not see.
//
// busConsumerUnits below now buckets TWO evidence sources, not one:
// busConsumerCalls (bus_dlq_test.go, unchanged — `bus.NewConsumer` call
// sites) AND directNATSClientSubscribeCalls (this file — direct
// `(*bus.NATSClient).Subscribe` / `SubscribeBroadcast` /
// `SubscribeBroadcastReady` call sites, matched by call SHAPE rather than by
// resolving the receiver's static type; see that function's own comment for
// why arity is precise enough here and what it deliberately does NOT match).
// A service present in either bucket is a JetStream consumer for this
// guard's purposes; the two evidence sources simply union.
func TestServiceHasJetStreamMachineryItsRoleRequires(t *testing.T) {
	root := moduleRoot(t)
	tenancyPath := filepath.Join(root, "infra", "nats", "tenancy.yaml")
	publishPerms := servicePublishPermissions(t, tenancyPath)
	subscribePerms := serviceSubscribeAllow(t, tenancyPath)
	consumerUnits := busConsumerUnits(t, root)
	rc := newResolveCache(root)

	var problems []string
	checked := 0

	// requireMachinery checks one __system__ user against the role(s) it was
	// derived to play. A user with no permissions block at all is skipped, not
	// re-reported: TestServicePublishesOnlySubjectsItsTenancyPermissionsAllow
	// (services) and TestOperatorCLIsHaveBrokerAccounts (operators) already
	// fail the build for that, and a second failure about the same root cause
	// would be noise, not signal.
	requireMachinery := func(name, svid string, consumes, publishes bool) {
		perm, ok := publishPerms[svid]
		if !ok {
			return
		}
		sub := subscribePerms[svid]
		checked++

		if consumes {
			if permDenies("$JS.API.>", perm) || !covered("$JS.API.>", perm.allow) {
				problems = append(problems, name+": consumes JetStream (via bus.NewConsumer or a direct (*bus.NATSClient) "+
					"Subscribe/SubscribeBroadcast/SubscribeBroadcastReady call) but tenancy.yaml's "+
					"permissions.publish for "+svid+" does not allow $JS.API.> — StreamNameBySubject and "+
					"CreateOrUpdateConsumer/CreateConsumer (pkg/bus/nats.go) publish under $JS.API.> to resolve the "+
					"stream and bind the consumer; without it the service authenticates fine and then fails every "+
					"Subscribe call at startup, before a single message is ever delivered")
			}
			if permDenies("$JS.ACK.>", perm) || !covered("$JS.ACK.>", perm.allow) {
				problems = append(problems, name+": consumes JetStream (via bus.NewConsumer or a direct (*bus.NATSClient) "+
					"Subscribe/SubscribeBroadcast/SubscribeBroadcastReady call) but tenancy.yaml's "+
					"permissions.publish for "+svid+" does not allow $JS.ACK.> — Msg.Ack() (pkg/bus/nats.go) "+
					"publishes to $JS.ACK.>, a separate subject space $JS.API.> does not cover, and Subscribe's own "+
					"error path only LOGS a failed Ack (logAckFailure) rather than surfacing one. The consumer will "+
					"process every message successfully and then be silently denied the ack, so the broker "+
					"redelivers the SAME message after AckWait forever and the handler reprocesses it forever, "+
					"while every health check the service exports keeps reporting green — the failure mode is "+
					"silent and looks like load, not like a permissions error")
			}
		}

		if !(consumes || publishes) {
			return
		}
		if covered("_INBOX.>", sub.allow) {
			return
		}
		if consumes {
			problems = append(problems, name+": consumes JetStream (via bus.NewConsumer or a direct (*bus.NATSClient) "+
				"Subscribe/SubscribeBroadcast/SubscribeBroadcastReady call) but tenancy.yaml's "+
				"permissions.subscribe for "+svid+" does not allow _INBOX.> — every $JS.API.> request "+
				"(StreamNameBySubject, CreateOrUpdateConsumer) and the Ack itself is a request/reply call that "+
				"needs its own reply inbox; without one those calls hang until they time out instead of failing "+
				"outright, so the consumer never actually resolves")
			return
		}
		problems = append(problems, name+": publishes to NATS but tenancy.yaml's permissions.subscribe for "+
			svid+" does not allow _INBOX.> — jetstream.PublishMsg (pkg/bus/nats.go's Publish) needs its own reply "+
			"inbox to receive the publish ack; without one every publish blocks for the full PublishTimeout and "+
			"then fails, even for a message the broker may already have stored durably")
	}

	for _, svc := range servicesWithEntrypoints(t, root) {
		if _, undeployed := notDeployed[svc]; undeployed {
			continue
		}
		if !dialsNATS(t, root, svc) {
			continue
		}
		publishes := len(servicePublishedSubjects(t, rc, root, svc)) > 0
		requireMachinery(svc, systemAccountSVID(svc), consumerUnits[svc], publishes)
	}

	for name, svid := range operatorSVIDs {
		dirs := []string{filepath.Join(root, "cmd", name)}
		for _, extra := range operatorCrossPackagePublishSurfaces[name] {
			dirs = append(dirs, filepath.Join(root, filepath.FromSlash(extra)))
		}
		publishes := len(publishedSubjectsIn(t, rc, dirs)) > 0
		requireMachinery(name, svid, consumerUnits[name], publishes)
	}

	// Read-only observers (nats_identity_test.go's readOnlyObserverSVIDs) are
	// cmd/ dialers too, and consume JetStream via bus.NewConsumer +
	// SubscribeBroadcast (so consumerUnits already buckets them). They publish NO
	// business subject — publishes=false — but a consumer STILL needs its own
	// $JS.API.>/$JS.ACK.>/_INBOX.> machinery, which is exactly what this loop
	// verifies. TestReadOnlyObserversCannotPublish asserts the OTHER half: that
	// the publish grant is machinery-only.
	for name, svid := range readOnlyObserverSVIDs {
		requireMachinery(name, svid, consumerUnits[name], false)
	}

	// Non-vacuity: this estate definitely has both roles represented — oms
	// alone is a bus.NewConsumer caller with its own permissions block, and
	// kanz-halt alone is a publish-only operator CLI with its own permissions
	// block. A scan that checked zero users would pass no matter how wrong
	// tenancy.yaml's machinery grants were.
	if checked == 0 {
		t.Fatal("checked zero __system__ users for JetStream machinery permissions — the scanner or the user list is broken")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d __system__ user(s) are missing NATS/JetStream machinery permissions their role requires:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// busConsumerUnits buckets every non-test JetStream-consumer call site this
// file knows about into the service or operator-CLI directory tree it
// belongs to — "services/<name>/..." or "cmd/<name>/...". A name present here
// rides the JetStream consumer path this guard requires machinery for, via
// EITHER of two evidence sources: busConsumerCalls (bus_dlq_test.go's
// `bus.NewConsumer` call sites — the original, and still the primary, path)
// or directNATSClientSubscribeCalls below (a direct `(*bus.NATSClient)`
// method call, bypassing `bus.NewConsumer` — the path archiver.go uses
// and the reason this second source exists at all). Reusing rather than
// re-deriving: no scanner here walks the module a third time looking for
// `bus.NewConsumer` sites; bucketSvcOrCmd is the one piece of bucketing logic
// both sources share.
func busConsumerUnits(t *testing.T, root string) map[string]bool {
	t.Helper()
	units := bucketSvcOrCmd(busConsumerCalls(t, root))
	for name := range bucketSvcOrCmd(directNATSClientSubscribeCalls(t, root)) {
		units[name] = true
	}
	return units
}

// bucketSvcOrCmd buckets a set of call sites (file paths relative to the
// module root) into the service or operator-CLI name owning the directory
// tree the call site lives in — "services/<name>/..." or "cmd/<name>/...".
// Shared by busConsumerUnits's two evidence sources so both bucket call
// sites the exact same way.
func bucketSvcOrCmd(calls []busConsumerCall) map[string]bool {
	units := map[string]bool{}
	for _, c := range calls {
		segs := strings.SplitN(c.file, "/", 3)
		if len(segs) < 2 {
			continue
		}
		switch segs[0] {
		case "services", "cmd":
			units[segs[1]] = true
		}
	}
	return units
}

// natsClientSubscribeMethodArity is the exact positional argument count each
// of bus.NATSClient's three subscribe methods takes (pkg/bus/nats.go):
//
//	Subscribe(ctx, subject, group string, h Handler) error                     — 4 args
//	SubscribeBroadcast(ctx, subject string, h Handler) error                    — 3 args
//	SubscribeBroadcastReady(ctx, subject string, h Handler, ready func()) error — 4 args
//
// directNATSClientSubscribeCalls matches on this arity rather than resolving
// the receiver's static type, because — like every other scanner in this
// file and bus_dlq_test.go — it does no go/types checking, only go/ast. See
// that function's comment for why arity is precise enough to avoid the real
// false positives this module contains.
var natsClientSubscribeMethodArity = map[string]int{
	"Subscribe":               4,
	"SubscribeBroadcast":      3,
	"SubscribeBroadcastReady": 4,
}

// directNATSClientSubscribeCalls finds every non-test call site anywhere in
// the module that invokes a method named Subscribe, SubscribeBroadcast, or
// SubscribeBroadcastReady with the exact argument count that method has on
// *bus.NATSClient — evidence that the caller holds a *bus.NATSClient (or a
// field/interface holding one, as services/archiver/internal/archive.Config's
// NATS field is: it's typed as a local `Subscriber` interface but populated
// with a real *bus.NATSClient at services/archiver/cmd/archiver/main.go's
// composition root) and is driving its JetStream consume-and-Ack path
// directly, bypassing bus.NewConsumer.
//
// WHY ARITY, AND WHY IT DOES NOT FALSE-POSITIVE ON THE OTHER Subscribe
// METHODS THIS MODULE ACTUALLY DEFINES (checked by grepping every `func (...)
// Subscribe` in the module before choosing this rule, not assumed):
//
//   - services/tv-sync/internal/projection.Projection.Subscribe(tenant,
//     accountID string) (<-chan Delta, func()) is an in-process fan-out for
//     the TradingView delta feed — nothing to do with NATS or JetStream. Its
//     one non-test call site (services/tv-sync/internal/brokerapi/brokerapi.go)
//     passes exactly 2 arguments; the 4-arg requirement for "Subscribe" above
//     excludes it without needing to know anything about its receiver's type.
//   - services/market-data/internal/feed's fakeBloomberg.Subscribe(ctx,
//     []string) (<-chan BloombergTick, error) is a test double and lives in a
//     _test.go file, which this scanner — like busConsumerCalls — skips
//     entirely regardless of arity.
//   - bus.KafkaClient also implements Subscribe(ctx, topic, group string, h
//     Handler) error: identical arity to bus.NATSClient's, and arity alone
//     cannot tell them apart. This is not a live ambiguity: the one Kafka
//     subscriber in this estate (lake-sink) reaches it through
//     bus.NewConsumer, not a direct call, so busConsumerCalls already marks
//     it a consumer — a KafkaClient.Subscribe match here would only be
//     REDUNDANT with a bucket that's already true, never a new false
//     positive. A future service calling (*bus.KafkaClient).Subscribe
//     directly, the way archiver calls (*bus.NATSClient).Subscribe, would
//     need its own tenancy.yaml JetStream-machinery grant re-derived at that
//     time regardless — Kafka and NATS permissions are not interchangeable —
//     so this scanner erring toward requiring it is the safe direction to be
//     wrong in, not a silent gap.
//   - `consumer.Subscribe(...)` / `consumer.SubscribeBroadcast(...)` /
//     `consumer.SubscribeBroadcastReady(...)` call sites on a *bus.Consumer
//     (the overwhelming majority of matches in services/ and cmd/) also match
//     this arity, and are also already true in busConsumerCalls's bucket via
//     the bus.NewConsumer call site that built that Consumer in the same
//     tree. Matching them again here is deliberately harmless duplication,
//     not a bug: bucketSvcOrCmd just ORs two `true` bits together.
func directNATSClientSubscribeCalls(t *testing.T, root string) []busConsumerCall {
	t.Helper()
	var out []busConsumerCall
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "gen", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
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
			wantArgs, known := natsClientSubscribeMethodArity[sel.Sel.Name]
			if !known || len(call.Args) != wantArgs {
				return true
			}
			out = append(out, busConsumerCall{
				file: filepath.ToSlash(rel),
				line: fset.Position(call.Pos()).Line,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return out
}

// operatorCrossPackagePublishSurfaces mirrors crossPackagePublishSurfaces
// (nats_service_permissions_test.go) for the operator plane: kanz-mandate's
// ConfigChanged FACT (compliance.mandate.changed.>) is emitted by
// internal/compliance/publisher.go's Publisher, constructed and called from
// cmd/kanz-mandate's composition root, not literally inside its main.go. The
// other three operator CLIs (kanz-halt, kanz-altevent, kanz-household)
// construct their bus.Event directly in their own cmd/ main.go — grep-
// verified — and need no entry here.
var operatorCrossPackagePublishSurfaces = map[string][]string{
	"kanz-mandate": {"internal/compliance"},
}
