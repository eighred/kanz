// Package shadowprocess bounds diagnostic subprocess I/O and lifetime. It does
// not provide an OS sandbox; deployment must independently deny network access.
package shadowprocess

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const MaxBytes = 1 << 20

type Config struct {
	Executable, Directory string
	Args, Environment     []string
	Timeout               time.Duration
}
type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		b.cancel()
		return 0, errors.New("worker output limit")
	}
	return b.buffer.Write(p)
}

func Run(ctx context.Context, c Config, input []byte) ([]byte, string) {
	if len(input) > MaxBytes {
		return nil, "invalid_input"
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	scratch, e := os.MkdirTemp("", "kanz-shadow-")
	if e != nil {
		return nil, "worker_failure"
	}
	defer os.RemoveAll(scratch)
	cmd := exec.CommandContext(ctx, c.Executable, c.Args...)
	cmd.Dir = scratch
	if c.Directory != "" {
		cmd.Dir = c.Directory
	}
	cmd.Env = append([]string{}, c.Environment...)
	cmd.Env = append(cmd.Env, "HOME="+scratch, "TMPDIR="+scratch, "TEMP="+scratch, "TMP="+scratch)
	if v := os.Getenv("SystemRoot"); v != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+v)
	}
	cmd.Stdin = bytes.NewReader(input)
	out := &boundedBuffer{limit: MaxBytes, cancel: cancel}
	stderr := &boundedBuffer{limit: 32 << 10, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	e = cmd.Run()
	if out.exceeded || stderr.exceeded {
		return nil, "output_limit"
	}
	if ctx.Err() != nil {
		return nil, "timeout"
	}
	if e != nil {
		return nil, "worker_failure"
	}
	return out.buffer.Bytes(), ""
}
