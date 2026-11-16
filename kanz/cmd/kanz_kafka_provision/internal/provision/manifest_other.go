//go:build !linux

package provision

// Native execution requires the Linux no-follow file boundary. Pure planning
// and state-machine tests remain portable; no production fallback opens input.
func ReadManifest(string) ([]byte, error) { return nil, ErrInput }
