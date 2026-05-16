//go:build windows

package terminal

// realSpawner on Windows always refuses — ConPty support is RFC P1 and
// the Desktop button + CLI both surface a clear error from this layer.
type realSpawner struct{}

func (realSpawner) Start(SpawnRequest) (PTY, error) { return nil, ErrUnsupportedOS }
