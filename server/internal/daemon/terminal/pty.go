package terminal

import (
	"io"
	"time"
)

// PTY abstracts the platform PTY + child process so tests can swap in a
// fake without forking a real shell. Read returns child stdout/stderr;
// Write delivers stdin; Resize updates the window; Wait blocks for the
// child to exit and returns its exit code; Close terminates the child
// and releases the master fd.
type PTY interface {
	io.ReadWriter
	Resize(cols, rows uint16) error
	Wait() (exitCode int, err error)
	Close() error
}

// SpawnRequest is the input to Spawner.Start.
type SpawnRequest struct {
	Shell   string
	Args    []string
	Cwd     string
	Env     []string
	Cols    uint16
	Rows    uint16
	Started time.Time
}

// Spawner creates new PTYs. Production uses realSpawner; tests inject a
// channel-backed fake (see fakePTY in manager_test.go).
type Spawner interface {
	Start(SpawnRequest) (PTY, error)
}
