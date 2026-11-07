package recoveryconformance

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Linux's per-process high-water mark is measured while the owned fixture is
// alive. It is not inferred from protobuf size or an algorithmic memory bound.
func (w *worker) peakMemory(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Fatal("actual large-history resource qualification requires Linux procfs")
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/status", w.cmd.Process.Pid))
	if err != nil {
		t.Fatal("owned process resource observation unavailable")
	}
	defer func() {
		if file.Close() != nil {
			t.Error("resource observation close failed")
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		t.Fatal("owned process resource observation exceeds bound")
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "VmHWM:" || fields[0] == "VmRSS:") {
			if len(fields) != 3 || fields[2] != "kB" {
				t.Fatal("invalid process memory units")
			}
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || n == 0 {
				t.Fatal("invalid process memory observation")
			}
			if _, exists := values[fields[0]]; exists {
				t.Fatal("duplicate process memory observation")
			}
			values[fields[0]] = n
		}
	}
	if values["VmHWM:"] < values["VmRSS:"] || values["VmRSS:"] == 0 {
		t.Fatal("incomplete process memory observation")
	}
	t.Logf("actual Rust recovery process memory: peak_rss_kib=%d current_rss_kib=%d", values["VmHWM:"], values["VmRSS:"])
}
