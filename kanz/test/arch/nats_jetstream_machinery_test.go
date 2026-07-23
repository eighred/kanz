package arch

import (
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
//     publish ack. It does NOT need `$JS.API.>` — proven against a real
//     broker in .superpowers/sdd/operator-svid-split-report.md: a
//     publish-only tool granted `$JS.API.>` anyway would hold the full
//     JetStream admin surface (stream/consumer create, delete, purge), a far
//     bigger privilege than PublishMsg needs or least-privilege calls for.
//   - A user may be both; the two requirements simply union (a consumer that
//     also publishes still only needs `_INBOX.>` once).
//
// SCOPE, HONESTLY: archiver calls `a.cfg.NATS.Subscribe` — the *NATSClient
// method — directly, never `bus.NewConsumer`, so it never appears in
// busConsumerUnits and this guard derives NO requirement for it (it also
// constructs no bus.Event, so it derives no PUBLISHES requirement either).
// That mirrors the exact boundary TestEveryBusConsumerWiresADLQ already draws
// around `bus.NewConsumer` call sites specifically, for the same reason: the
// task was to reuse that evidence, not to build a second, wider notion of
// "consumes." Whether archiver's own direct-Subscribe path needs the same
// machinery is a real question (nats.go's Subscribe method is the same code
// whoever calls it) but a distinct, not-yet-scoped one — see this file's
// mutation-proof notes in the task report for why widening it here was left
// alone rather than guessed at.
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
				problems = append(problems, name+": consumes via bus.NewConsumer (JetStream) but tenancy.yaml's "+
					"permissions.publish for "+svid+" does not allow $JS.API.> — StreamNameBySubject and "+
					"CreateOrUpdateConsumer/CreateConsumer (pkg/bus/nats.go) publish under $JS.API.> to resolve the "+
					"stream and bind the consumer; without it the service authenticates fine and then fails every "+
					"Subscribe call at startup, before a single message is ever delivered")
			}
			if permDenies("$JS.ACK.>", perm) || !covered("$JS.ACK.>", perm.allow) {
				problems = append(problems, name+": consumes via bus.NewConsumer (JetStream) but tenancy.yaml's "+
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
			problems = append(problems, name+": consumes via bus.NewConsumer (JetStream) but tenancy.yaml's "+
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

// busConsumerUnits buckets every non-test bus.NewConsumer call site
// busConsumerCalls (bus_dlq_test.go) already finds into the service or
// operator-CLI directory tree it belongs to — "services/<name>/..." or
// "cmd/<name>/...". A name present here calls bus.NewConsumer somewhere in
// its own tree and therefore rides the JetStream consumer path this guard
// requires machinery for. This is the reuse the task asked for: no second AST
// scanner walks the module looking for bus.NewConsumer, this one just buckets
// the same call sites bus_dlq_test.go already found by directory.
func busConsumerUnits(t *testing.T, root string) map[string]bool {
	t.Helper()
	units := map[string]bool{}
	for _, c := range busConsumerCalls(t, root) {
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
