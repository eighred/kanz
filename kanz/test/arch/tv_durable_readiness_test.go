package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

var tvReadinessMarkers = map[string][]string{
	"pkg/bus/bus.go":                                       {"type ReadySubscriber interface"},
	"pkg/bus/consumer.go":                                  {"effective.DurableConsumerTuning(subject)", "tuning.MaxDeliver != -1", "c.subscribe(ctx, subject, group, h, nil)", "c.subscribe(ctx, subject, group, h, ready)", "transport lacks durable readiness"},
	"pkg/bus/nats.go":                                      {"tuning := tuningForDurable(subject, c.cfg.ConsumerTuning)", "c.subscribeDurable(ctx, subject, group, h, nil)", "c.subscribeDurable(ctx, subject, group, h, ready)", "info.NumPending == 0 && info.NumAckPending == 0", "waitInitialCatchup(ctx, cons, ready)"},
	"services/tv-sync/cmd/tv-sync/main.go":                 {"validateFactPriceSeparation(cfg.PriceSubjects)", "bus.RetainedWorkTuning()", "ConsumerTuning: &factTuning", "foldHandler := guardedFold(readiness, cfg.Tenant, proj.Handle)", "subs[s] = foldHandler", "server.NewReadiness(ctx)", "cfg.ConsumerGroup, readiness.Set", "consumer.SubscribeReady(ctx, subject, group, handler, caughtUp)", "if remaining == 0", "setReady(false)", "if stopped || ctx.Err() != nil"},
	"services/tv-sync/internal/server/server.go":           {"broker.SetStreamGate(readiness)", "context.AfterFunc(ctx, r.revokeStreams)", "r.revokeStreams()", "!r.foldFailures.blocked.Load()", "r.ctx.Err() == nil", "!s.readiness.Ready()", "s.serveBroker(w, r, s.mux)", "s.serveBroker(w, r, s.legacy)"},
	"test/arch/nats_subscribe_permissions_test.go":         {"\"SubscribeReady\":"},
	"test/arch/nats_jetstream_machinery_test.go":           {"\"SubscribeReady\":"},
	"test/arch/nats_identity_test.go":                      {"sel.Sel.Name == \"SubscribeReady\" && len(call.Args) == 5"},
	"../.github/workflows/kanz-ci.yml":                     {"check_tv_readiness_evidence.py", "TEST_NATS_URL: nats://localhost:4222"},
	"services/tv-sync/cmd/tv-sync/fact_consumers.go":       {"bus.WithRetainedRetries(projection.Subjects()...)", "bus.RequireTenantScope", "env.GetTenantId() != tenant", "attempts.Lock()", "defer attempts.Unlock()", "readiness.FoldResult", "failed := true", "r.facts.SubscribeReady", "r.prices.SubscribeReady", `root == "order"`, `strings.ContainsAny(root, "*>")`},
	"services/tv-sync/internal/server/fold_readiness.go":   {"maxUnresolvedFolds = 4096", "sha256.Sum256", "f.overflow = true", "delete(f.pending, key)", "f.overflow || len(f.pending) > 0"},
	"services/tv-sync/internal/server/stream_readiness.go": {"l.generation.closed", "l.r.generation != l.generation", "!l.r.Ready()", "close(r.generation.done)"},
	"services/tv-sync/internal/brokerapi/brokerapi.go":     {"h.streamGate == nil", "h.streamGate.AcquireStream()", "case <-lease.Done()", "return lease.Write", "time.Now().Add(5 * time.Second)", "if !started"},
}

func tvReadinessBoundary(files map[string]string) error {
	for name, markers := range tvReadinessMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[name], marker) {
				return fmt.Errorf("%s missing %q", name, marker)
			}
		}
	}
	return nil
}
func TestTVDurableReadinessKeepsSharedDeliveryAndClosedAPI(t *testing.T) {
	root := moduleRoot(t)
	files := map[string]string{}
	for name := range tvReadinessMarkers {
		files[name] = string(mustReadArchFile(t, filepath.Join(root, name)))
	}
	if err := tvReadinessBoundary(files); err != nil {
		t.Fatal(err)
	}
	for name, markers := range tvReadinessMarkers {
		for _, marker := range markers {
			t.Run(name+marker, func(t *testing.T) {
				changed := map[string]string{}
				for k, v := range files {
					changed[k] = v
				}
				changed[name] = strings.ReplaceAll(changed[name], marker, "REMOVED")
				if err := tvReadinessBoundary(changed); err == nil {
					t.Fatal("readiness mutation escaped")
				}
			})
		}
	}
}

func checkTVDurableReadinessCI(data []byte) error {
	if err := checkTVProofCI(data, "check_tv_readiness_evidence.py --pattern", "TEST_POSTGRES_URL", "postgres://kanzapp:kanz@localhost:5432/kanzapp?sslmode=disable", nil); err != nil {
		return err
	}
	return checkTVProofCI(data, "check_tv_readiness_evidence.py --pattern", "TEST_NATS_URL", "nats://localhost:4222", []string{
		"set -euo pipefail",
		"python3 -B -m unittest discover -s ../.github/scripts -p test_check_tv_readiness_evidence.py",
		`go test -race -p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_readiness_evidence.py --pattern)" ./pkg/bus ./services/tv-sync/cmd/tv-sync ./services/tv-sync/internal/server ./services/tv-sync/internal/brokerapi | tee "$RUNNER_TEMP/tv-readiness-proof.jsonl"`,
		`python3 ../.github/scripts/check_tv_readiness_evidence.py "$RUNNER_TEMP/tv-readiness-proof.jsonl"`,
	})
}
func TestTVDurableReadinessRequiresActualBrokerEvidence(t *testing.T) {
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := checkTVDurableReadinessCI(data); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{`-race -p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_readiness_evidence.py --pattern)"`, `-p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_readiness_evidence.py --pattern)"`},
		{`python3 ../.github/scripts/check_tv_readiness_evidence.py "$RUNNER_TEMP/tv-readiness-proof.jsonl"`, "echo unchecked"},
		{"check_tv_readiness_evidence.py --pattern", "check_tv_readiness_evidence.py --unused"},
	} {
		mutated := strings.Replace(string(data), change[0], change[1], 1)
		if mutated == string(data) {
			t.Fatal("mutation missed")
		}
		if checkTVDurableReadinessCI([]byte(mutated)) == nil {
			t.Fatal("weakened readiness proof accepted")
		}
	}
}
