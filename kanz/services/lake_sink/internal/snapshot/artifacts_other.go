//go:build !linux

package snapshot

import "context"

func publishArtifactFiles(context.Context, string, string, []byte, []byte) error { return ErrCustody }
func readArtifactFile(context.Context, string, string, string, int) ([]byte, error) {
	return nil, ErrCustody
}
