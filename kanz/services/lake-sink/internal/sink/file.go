package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileSink lands rows as Hive-partitioned newline-delimited JSON under a root
// directory: {root}/{domain}/{entity}/dt=YYYY-MM-DD/part-{instance}.ndjson. This
// is the staging layout a lakehouse catalog ingest (Iceberg/Delta via
// Trino/Spark) commits as a table — the columns evolve as the decoded payload
// does, which is the LAKE-01a schema-evolution property. Each process writes its
// own part-file (keyed by instance) so concurrent replicas never interleave into
// one file.
//
// Concrete Iceberg/Delta catalog commits (manifest writes, snapshot isolation)
// are a deployment concern that plugs in behind the Sink interface; the
// NDJSON-on-object-store landing keeps that boundary clean and avoids a heavy
// table-format dependency in the service binary.
type FileSink struct {
	root       string
	instanceID string

	mu      sync.Mutex
	writers map[string]*partWriter
}

type partWriter struct {
	f  *os.File
	bw *bufio.Writer
}

// NewFileSink creates the landing root. instanceID disambiguates this process's
// part-files from sibling replicas'; empty ⇒ hostname+pid.
func NewFileSink(root, instanceID string) (*FileSink, error) {
	if instanceID == "" {
		instanceID = defaultInstanceID()
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &FileSink{root: root, instanceID: sanitize(instanceID), writers: map[string]*partWriter{}}, nil
}

func (s *FileSink) Write(_ context.Context, row Row) error {
	line, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("marshal row: %w", err)
	}
	dir := s.partitionDir(row)

	s.mu.Lock()
	defer s.mu.Unlock()
	pw, err := s.writerFor(dir)
	if err != nil {
		return err
	}
	if _, err := pw.bw.Write(line); err != nil {
		return err
	}
	return pw.bw.WriteByte('\n')
}

// Flush makes every buffered writer durable (buffer → fd → disk).
func (s *FileSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, pw := range s.writers {
		if err := pw.flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close flushes and closes every open part-file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for dir, pw := range s.writers {
		if err := pw.flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := pw.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.writers, dir)
	}
	return firstErr
}

func (pw *partWriter) flush() error {
	if err := pw.bw.Flush(); err != nil {
		return err
	}
	return pw.f.Sync()
}

// writerFor returns (opening if needed) the part-file writer for a partition
// directory. Caller holds s.mu.
func (s *FileSink) writerFor(dir string) (*partWriter, error) {
	if pw, ok := s.writers[dir]; ok {
		return pw, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "part-"+s.instanceID+".ndjson")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	pw := &partWriter{f: f, bw: bufio.NewWriter(f)}
	s.writers[dir] = pw
	return pw, nil
}

func (s *FileSink) partitionDir(row Row) string {
	day := row.EventTime
	if day.IsZero() {
		day = row.IngestedAt
	}
	return filepath.Join(
		s.root,
		sanitize(row.Domain),
		sanitize(row.Entity),
		"dt="+day.UTC().Format("2006-01-02"),
	)
}

// sanitize keeps a path segment to [A-Za-z0-9_.-], folding everything else to
// '_' so a domain/entity value can never escape the landing root or inject a
// separator. Empty ⇒ "unknown".
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "unknown"
	}
	return out
}

func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
