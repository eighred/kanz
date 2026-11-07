package snapshot

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type snapshotImageState struct {
	Running bool
	Exit    int
	OOM     bool
}

// Closed diagnostic projection only; no raw Docker fields or errors are logged.
func snapshotImageDiagnostic(raw []byte, id, nonce, run string) (snapshotImageState, bool) {
	var v struct {
		ID      string `json:"id"`
		Nonce   string `json:"nonce"`
		Run     string `json:"run"`
		Running *bool  `json:"running"`
		Exit    *int   `json:"exit"`
		OOM     *bool  `json:"oom"`
	}
	if len(raw) == 0 || len(raw) > 4096 {
		return snapshotImageState{}, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.ID != id || v.Nonce != nonce || v.Run != run || v.Running == nil || v.Exit == nil || v.OOM == nil || *v.Exit < 0 || *v.Exit > 255 {
		return snapshotImageState{}, false
	}
	return snapshotImageState{*v.Running, *v.Exit, *v.OOM}, true
}

func TestSnapshotImageStartupDiagnosticProjection(t *testing.T) {
	testSnapshotStartupMessages(t)
	valid := []byte(`{"id":"owned","nonce":"nonce","run":"run","running":false,"exit":2,"oom":false}`)
	state, ok := snapshotImageDiagnostic(valid, "owned", "nonce", "run")
	if !ok || state.Running || state.Exit != 2 || state.OOM {
		t.Fatal("closed startup state differs")
	}
	for _, raw := range [][]byte{
		bytes.Replace(valid, []byte(`"owned"`), []byte(`"other"`), 1),
		bytes.Replace(valid, []byte(`"nonce":"nonce"`), []byte(`"nonce":"other"`), 1),
		bytes.Replace(valid, []byte(`"run":"run"`), []byte(`"run":"other"`), 1),
		bytes.Replace(valid, []byte(`,"oom":false`), nil, 1),
		bytes.Replace(valid, []byte(`"exit":2`), []byte(`"exit":256`), 1),
		bytes.Replace(valid, []byte(`"running":false`), []byte(`"running":"false"`), 1),
		bytes.Replace(valid, []byte(`"oom":false`), []byte(`"oom":false,"env":"forbidden"`), 1),
		append(append([]byte{}, valid...), []byte(`{}`)...), bytes.Repeat([]byte("x"), 4097),
	} {
		if _, ok := snapshotImageDiagnostic(raw, "owned", "nonce", "run"); ok {
			t.Fatal("unbound or malformed startup diagnostic accepted")
		}
	}
}

// Only fixed categories leave this collector. A Docker tail never proves that
// earlier messages were absent, even when transport collection is complete.
type snapshotStartupMessages struct {
	Bits           uint16
	Counts         [10]uint16
	Complete       bool
	Unknown        bool
	HistoryUnknown bool
}

type snapshotStartupCapture struct {
	mu       sync.Mutex
	raw      []byte
	overflow bool
}

func (c *snapshotStartupCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	left := (64 << 10) - len(c.raw)
	if len(p) > left {
		c.overflow = true
		p = p[:left]
	}
	c.raw = append(c.raw, p...)
	return n, nil // discard excess while the bounded command drains/joins
}

var snapshotStartupText = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} ERROR (config load failed|observability init failed) err=`)

func snapshotStartupCategory(line []byte) (int, bool) {
	if len(line) == 0 || len(line) > 8<<10 {
		return 0, false
	}
	if m := snapshotStartupText.FindSubmatch(line); m != nil {
		if string(m[1]) == "config load failed" {
			return 0, true
		}
		return 1, true
	}
	d := json.NewDecoder(bytes.NewReader(line))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return 0, false
	}
	seen := map[string]bool{}
	msg, level := "", ""
	for d.More() {
		key, err := d.Token()
		k, ok := key.(string)
		if err != nil || !ok || seen[k] {
			return 0, false
		}
		seen[k] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return 0, false
		}
		switch k {
		case "msg":
			if json.Unmarshal(value, &msg) != nil {
				return 0, false
			}
		case "level":
			if json.Unmarshal(value, &level) != nil {
				return 0, false
			}
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return 0, false
	}
	messages := [...]string{"config load failed", "observability init failed", "sink open failed", "http server failed", "sink stopped with error", "sink close error", "http shutdown error", "lake-sink listening", "bus transport", "lake-sink subscribing"}
	// The first two are emitted only by the pre-initialization default logger.
	for i := 2; i < len(messages); i++ {
		expected := "ERROR"
		if i >= 7 {
			expected = "INFO"
		}
		if msg == messages[i] && level == expected {
			return i, true
		}
	}
	return 0, false
}

func (c *snapshotStartupCapture) finish(success bool) snapshotStartupMessages {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := snapshotStartupMessages{Complete: success && !c.overflow, HistoryUnknown: true}
	if c.overflow {
		result.Unknown = true
	}
	lines := bytes.Split(c.raw, []byte{'\n'})
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			continue
		}
		if i == len(lines)-1 {
			result.Complete = false
			result.Unknown = true
			continue
		}
		category, ok := snapshotStartupCategory(line)
		if !ok {
			result.Unknown = true
			continue
		}
		result.Bits |= 1 << category
		result.Counts[category]++
	}
	clear(c.raw)
	c.raw = nil
	return result
}

// Ownership is checked before invoking the supplied bounded collector.
func snapshotOwnedStartupMessages(raw []byte, id, nonce, run string, collect func() snapshotStartupMessages) snapshotStartupMessages {
	if _, valid := snapshotImageDiagnostic(raw, id, nonce, run); !valid {
		return snapshotStartupMessages{Unknown: true, HistoryUnknown: true}
	}
	return collect()
}

func testSnapshotStartupMessages(t *testing.T) {
	secret := "SECRET_SENTINEL_NOT_A_CATEGORY"
	var c snapshotStartupCapture
	_, _ = c.Write([]byte(`{"level":"ERROR","msg":"sink open failed","err":"` + secret + `","argv":["` + secret + `"]}` + "\n"))
	_, _ = c.Write([]byte("2026/11/07 12:34:56 ERROR config load failed err=" + secret + "\n"))
	result := c.finish(true)
	encoded, _ := json.Marshal(result)
	if result.Bits != 5 || result.Counts[0] != 1 || result.Counts[2] != 1 || !result.Complete || result.Unknown || !result.HistoryUnknown || strings.Contains(string(encoded), secret) || c.raw != nil {
		t.Fatal("startup projection differs")
	}
	for _, line := range []string{
		`{"level":"ERROR","msg":"sink open failed","msg":"sink stopped with error"}`,
		`{"level":"INFO","msg":"sink open failed"}`,
		`{"level":"ERROR","msg":"prefix sink open failed"}`,
		`{"level":"ERROR","msg":"sink open failed"}{}`,
		`{"level":"ERROR","msg":"unknown","err":"sink open failed"}`,
		"2026/11/07 12:34:56 ERROR config load failed suffix err=secret",
		"prefix 2026/11/07 12:34:56 ERROR config load failed err=secret",
		strings.Repeat("x", (8<<10)+1),
	} {
		if _, ok := snapshotStartupCategory([]byte(line)); ok {
			t.Fatal("noncanonical startup message accepted")
		}
	}
	var large snapshotStartupCapture
	_, _ = large.Write(bytes.Repeat([]byte("x"), (64<<10)+1))
	if len(large.raw) != 64<<10 {
		t.Fatal("capture byte cap differs")
	}
	if v := large.finish(true); v.Complete || !v.Unknown {
		t.Fatal("truncated capture accepted")
	}
	var incomplete snapshotStartupCapture
	_, _ = incomplete.Write([]byte(`{"level":"ERROR","msg":"sink open failed"}`))
	if v := incomplete.finish(false); v.Complete || !v.Unknown || v.Bits != 0 {
		t.Fatal("incomplete capture accepted")
	}
	var concurrent snapshotStartupCapture
	var writers sync.WaitGroup
	for range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 20 {
				_, _ = concurrent.Write([]byte(`{"level":"INFO","msg":"bus transport","err":"SECRET_SENTINEL_NOT_A_CATEGORY"}` + "\n"))
			}
		}()
	}
	writers.Wait()
	if v := concurrent.finish(true); !v.Complete || v.Unknown || v.Counts[8] != 40 {
		t.Fatal("concurrent channels lost bounded category records")
	}
	validOwner := []byte(`{"id":"owned","nonce":"nonce","run":"run","running":false,"exit":1,"oom":false}`)
	invoked := false
	owned := snapshotOwnedStartupMessages(validOwner, "owned", "nonce", "run", func() snapshotStartupMessages {
		invoked = true
		return snapshotStartupMessages{Bits: 4, HistoryUnknown: true}
	})
	if !invoked || owned.Bits != 4 {
		t.Fatal("owned diagnostic did not collect")
	}
	called := false
	v := snapshotOwnedStartupMessages([]byte(`{}`), "owned", "nonce", "run", func() snapshotStartupMessages { called = true; return snapshotStartupMessages{} })
	if called || !v.Unknown || v.Complete {
		t.Fatal("foreign ownership invoked log collector")
	}
}
