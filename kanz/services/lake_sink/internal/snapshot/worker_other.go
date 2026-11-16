//go:build !linux || !amd64

package snapshot

import "os"

func openWorkerExecutable(string, [32]byte) (*os.File, error) { return nil, ErrWorker }
