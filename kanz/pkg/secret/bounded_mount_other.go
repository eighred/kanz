//go:build !linux

package secret

import (
	"errors"
	"os"
)

// This operator API requires a tested nonblocking descriptor-open primitive.
// Legacy Read remains portable; do not emulate safety with a racy pre-Stat.
func openBoundedMount(string) (*os.File, error) {
	return nil, errors.New("secret: bounded mounts require Linux")
}
