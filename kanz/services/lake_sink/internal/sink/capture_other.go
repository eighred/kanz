//go:build !linux

package sink

import "os"

type captureRoot struct{ device, inode uint64 }

func openCaptureRoot(string) (*captureRoot, error)            { return nil, ErrCapture }
func (*captureRoot) close() error                             { return ErrCapture }
func (*captureRoot) create(string) (*os.File, error)          { return nil, ErrCapture }
func (*captureRoot) open(string) (*os.File, error)            { return nil, ErrCapture }
func captureIdentity(*os.File) (uint64, uint64, int64, error) { return 0, 0, 0, ErrCapture }
