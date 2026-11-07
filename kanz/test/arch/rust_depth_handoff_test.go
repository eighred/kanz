package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a recipe/ownership guard, not proof that a child authenticated or a
// handoff completed. The named Linux process test must execute separately.
func TestRustFeedHandoffFixtureBoundary(t *testing.T) {
	markers := map[string][]string{
		"handoff_transport_linux_test.go": {
			"//go:build linux", "TLSMap: true", "tls.VersionTLS13", "tlsconfig.AuthorizeID(spiffeid.RequireFromString(feedBrokerID))",
			"c.AuthorizedUser == id", "exec.CommandContext(ctx, binary)", "Setpgid: true", "syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)",
			"p.done <- cmd.Wait()", "p.source.Active() == 0 && len(p.broker.connections(p.principal)) == 0", "cmd.Env = []string{",
			"Username: feedObserverID, Permissions: permissions(\"$JS.API.>\", \"$JS.ACK.>\")", "observer-must-not-publish",
			"rustPermissions.Publish = &ns.SubjectPermission{Deny: []string{\">\"}}", "strings.HasSuffix(format, \"Publish Violation - Subject %q\")",
			"p.done <- err // Preserve the one Wait result", "phase != \"denied-startup\" && phase != \"go-startup\" && phase != \"rust-startup\"",
		},
		"handoff_integration_linux_test.go": {
			"TestFeedRuntimeControlledHandoff", "KANZ_MARKET_INGEST_BIN", "KANZ_BINANCE_DEPTH_BIN", "KANZ_OKX_DEPTH_BIN",
			"rustEnv, false).refused()", "original.stop()", "retained := startGo(\"go-retained\", true)", "rust.stop()", "retained.stop()",
			"rollback := startGo(\"go-rollback\", false)", "e.ProducerVersion != version", "bus.Validate(frame.GetEnvelope())",
			"GetSource() == \"go-retained\"", "broker.server.DisconnectClientByID(ids[0])",
			"info.State.FirstSeq != 1", "info.State.Msgs != info.State.LastSeq", "info.State.NumDeleted != 0",
			"authenticated-publish-denied", "deniedBroker.denied.publish.Load() > beforeDenials", "denied publisher produced accepted facts",
			"duplicate-external-assignment-refused", "instrument+\",\"+instrument", "invalid static assignment reached feed side effects",
			"rustSource := venue + \"-depth/handoff\"", "depth(rustSource, symbol, 101)", "depth(\"go-original\", instrument, 101)", "depth(\"go-rollback\", instrument, 101)", "Durable: consumer", "nats.Bind(\"HANDOFF\", consumer)",
			"broker.js.DeleteConsumer(\"HANDOFF\", consumer)", "!errors.Is(err, nats.ErrConsumerNotFound)", "handoffBrokerError(consumerErr)",
			"depth(rustSource, symbol, 102)", "depth(rustSource, symbol, 105)", "depth(rustSource, symbol, 106)",
			"requests, release := exchange.holdAnchor(104)", "exchange.anchorRequests() == requests+1", "exchange.connections(\"depth\") != connections", "exchange.resetAnchor()",
			"book.LastUpdateSequence != sequence", "event.SourceSequence != sequence",
			"handoffStatus(t, rustAddress, http.StatusServiceUnavailable)\n\t\t\t\trelease()",
			"expected = append(expected, previous)", "expected = append(expected, bucket)", "carryover[source] = bucket.Add(time.Minute)", "!b.OpenTime.AsTime().Equal(want)",
		},
		"handoff_exchange_linux_test.go": {
			"x.wg.Add(1)\n\t\tdefer x.wg.Done()\n\t\tx.restRequests++", "sequence, gate := x.restSequence, x.restGate", "x.restRequests++", "timer := time.NewTimer(5 * time.Second)", "case <-r.Context().Done():", "case <-timer.C:",
			"x.t.Cleanup(release)", "once.Do(func() { close(gate) })",
			"if snapshot {\n\t\t\taction = \"snapshot\"\n\t\t\tprevious = -1\n\t\t}",
		},
	}
	root := filepath.Join(moduleRoot(t), "tools", "depth-conformance")
	for file, required := range markers {
		body, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		check := func(source string) error {
			for _, marker := range required {
				if !strings.Contains(source, marker) {
					return fmt.Errorf("required fixture boundary absent: %s", marker)
				}
			}
			if strings.Contains(source, "os.Environ()") {
				return fmt.Errorf("child inherited ambient authority")
			}
			return nil
		}
		normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
		if err := check(normalized); err != nil {
			t.Fatal(err)
		}
		for _, marker := range required {
			if check(strings.ReplaceAll(normalized, marker, "removed-boundary")) == nil {
				t.Fatalf("%s mutation escaped", file)
			}
		}
		if check(normalized+"\n// os.Environ()") == nil {
			t.Fatal("ambient environment mutation escaped")
		}
	}
}

func TestRustFeedHandoffQualificationRecipe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), "..", ".github", "workflows", "architecture-migration.yml"))
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{
		`KANZ_BUILD_VERSION="$source" CARGO_TARGET_DIR="$RUNNER_TEMP/feed-handoff-target" cargo +1.90.0 build --locked`,
		`go build -trimpath -ldflags "-X github.com/eighred/kanz/internal/version.stamped=$source"`,
		`go test -race -p 1 -c -o "$RUNNER_TEMP/feed-handoff-bin/handoff.test"`,
		`-test.count=1 -test.timeout=8m -test.run '^TestFeedRuntimeControlledHandoff$'`,
		`KANZ_FEED_HANDOFF_VERSION: ${{ github.sha }}`,
		`check_feed_handoff_evidence.py --prepare --root .`,
		`check_feed_handoff_evidence.py --root ..`,
		`name: feed-runtime-handoff`,
		`path: ${{ runner.temp }}/feed-handoff-proof/`,
	}
	check := func(source string) bool {
		for _, marker := range markers {
			if !strings.Contains(source, marker) {
				return false
			}
		}
		return true
	}
	if !check(string(body)) {
		t.Fatal("feed handoff qualification recipe incomplete")
	}
	for _, marker := range markers {
		if check(strings.ReplaceAll(string(body), marker, "removed")) {
			t.Fatal("qualification removal mutation escaped")
		}
	}
}
