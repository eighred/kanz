// Package provision owns the optional native tenant Kafka provisioning command.
// It never executes the shell program surrounding the tracked topic table.
package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/eighred/kanz/internal/topic"
)

const MaxManifestBytes = 128 << 10

var ErrInput = errors.New("invalid provisioning input")

var topicName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
var tenantName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type Topic struct {
	Name       string `json:"name"`
	Partitions int    `json:"partitions"`
	Cleanup    string `json:"cleanup"`
	Retention  int64  `json:"retention_ms"`
}

// ParseTable accepts the exact bounded manifest bytes whose digest the caller
// pinned in its Job. Only the single, literal topic-table heredoc is data;
// shell commands, interpolation, arbitrary YAML and includes are not evaluated.
func ParseTable(raw []byte, expectedSHA, tenant string) ([]Topic, error) {
	if len(raw) == 0 || len(raw) > MaxManifestBytes || (tenant != topic.SystemTenant && !tenantName.MatchString(tenant)) {
		return nil, ErrInput
	}
	digest := sha256.Sum256(raw)
	if len(expectedSHA) != 64 || hex.EncodeToString(digest[:]) != expectedSHA {
		return nil, ErrInput
	}
	lines := strings.Split(string(raw), "\n")
	inside, closed := false, false
	seen := make(map[string]bool)
	rowNames := make(map[string]bool)
	var out []Topic
	appendTopic := func(t Topic) bool {
		if seen[t.Name] || len(out) >= 256 {
			return false
		}
		seen[t.Name] = true
		out = append(out, t)
		return true
	}
	rows := 0
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "    done <<'EOF'" {
			if inside || closed {
				return nil, ErrInput
			}
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if line == "    EOF" {
			inside, closed = false, true
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			return nil, ErrInput
		}
		line = strings.TrimSpace(line[4:])
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 5 || !topicName.MatchString(f[0]) || (f[2] != "delete" && f[2] != "compact") || (f[4] != "yes" && f[4] != "no") {
			return nil, ErrInput
		}
		partitions, e1 := strconv.Atoi(f[1])
		retention, e2 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || partitions < 1 || partitions > 128 || (retention != -1 && retention <= 0) {
			return nil, ErrInput
		}
		rows++
		if rowNames[f[0]] {
			return nil, ErrInput
		}
		rowNames[f[0]] = true
		// The existing tenant-table guard owns this sole system-only exemption.
		if f[0] == "dlq.archiver" && tenant != topic.SystemTenant {
			continue
		}
		if !appendTopic(Topic{topic.Qualify(tenant, f[0]), partitions, f[2], retention}) {
			return nil, ErrInput
		}
		if f[4] == "yes" && !appendTopic(Topic{topic.Qualify(tenant, "dlq."+f[0]), 3, "delete", 2592000000}) {
			return nil, ErrInput
		}
	}
	if inside || !closed || rows == 0 || !seen[topic.Qualify(tenant, "order.order")] {
		return nil, ErrInput
	}
	return out, nil
}
