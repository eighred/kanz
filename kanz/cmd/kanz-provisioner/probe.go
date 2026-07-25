package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// probeDialTimeout bounds the single dial. Short because a human is waiting on the
// Add Node form for the answer, and because "did not answer within 10s" is the same
// operational verdict as "refused" — the target is not ready to be provisioned.
const probeDialTimeout = 10 * time.Second

// terminationLogPath is where Kubernetes reads a finished container's termination
// message from, and it is the ONLY channel this mode reports through: the operator
// reads pod status, which it can already do, rather than pod logs, which would be a
// far wider RBAC grant than one line of status is worth. A var, not a const, so a test
// can point it at a temp file; not a flag, because the Job spec leaves
// terminationMessagePath at its default and this must match it.
var terminationLogPath = "/dev/termination-log"

// runProbe dials PROVISION_TARGET_ADDR once and reports whether it answered.
//
// It requires NO credentials — not K3S_SERVER_URL, not K3S_TOKEN, not an SSH user,
// not the bootstrap key. That is deliberate: demanding them would force the caller to
// mint and mount a bootstrap key into a pod whose whole job is to open a socket and
// close it, putting key material on a path any caller can trigger at will.
//
// Nothing is ever read from the connection. The verdict is reachability and latency;
// peer bytes are not the operator's business and must not become a way to read one.
func runProbe() error {
	addr := os.Getenv("PROVISION_TARGET_ADDR") // "ip:port"
	if addr == "" {
		return fmt.Errorf("missing required env (PROVISION_TARGET_ADDR)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeDialTimeout)
	defer cancel()

	start := time.Now()
	d := net.Dialer{Timeout: probeDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// Unreachable is a RESULT, not a malfunction. It still exits non-zero so the
		// Job records a failure and `kubectl get job` tells the same story the
		// termination message does — the operator reads the message, a human reading
		// the namespace afterwards reads the exit code.
		reason := dialReason(err)
		writeProbeResult("unreachable " + reason)
		return fmt.Errorf("unreachable %s: %s", addr, reason)
	}
	_ = conn.Close()
	writeProbeResult(fmt.Sprintf("reachable %d", time.Since(start).Milliseconds()))
	return nil
}

// writeProbeResult writes the one line the operator parses.
//
// A write failure is reported on stderr and changes nothing else: the exit code's
// contract is reachability, and the operator treats a missing or unparseable message
// as a broken probe rather than an unreachable host, so the failure already surfaces
// as itself without a second, conflicting signal.
func writeProbeResult(line string) {
	if err := os.WriteFile(terminationLogPath, []byte(line+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-provisioner: write termination message: "+err.Error())
	}
}

// dialReason trims a dial error to its last ": "-delimited segment — the short,
// client-safe tail ("connection refused", "i/o timeout"). The full error names the
// dialled address, which the caller supplied and does not need echoed back, wrapped in
// Go-specific noise no operator reads for meaning.
func dialReason(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}
