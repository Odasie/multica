//go:build !windows

package terminal

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// realSpawner forks the shell on a PTY using creack/pty. Linux/macOS only;
// Windows reaches the stub in spawner_windows.go and returns ErrUnsupportedOS.
type realSpawner struct{}

func (realSpawner) Start(req SpawnRequest) (PTY, error) {
	cmd := exec.Command(req.Shell, req.Args...)
	cmd.Dir = req.Cwd

	// Inherit the daemon's PATH so users get whatever CLIs are installed
	// in the daemon's environment (claude, codex, multica, etc.); merge
	// in the per-session vars built by buildEnv.
	env := os.Environ()
	env = append(env, req.Env...)
	cmd.Env = env

	size := &pty.Winsize{Cols: req.Cols, Rows: req.Rows}
	f, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, fmt.Errorf("pty.StartWithSize: %w", err)
	}
	return &unixPTY{cmd: cmd, file: f}, nil
}

type unixPTY struct {
	cmd  *exec.Cmd
	file *os.File
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.file.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.file.Write(b) }

func (p *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(p.file, &pty.Winsize{Cols: cols, Rows: rows})
}

func (p *unixPTY) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), err
	}
	return -1, err
}

func (p *unixPTY) Close() error {
	if p.cmd.Process != nil {
		// SIGHUP gives interactive shells a chance to clean up before the
		// fd disappears. The Wait goroutine in PtySession picks up the
		// subsequent exit and broadcasts it.
		_ = p.cmd.Process.Signal(os.Interrupt)
		_ = p.cmd.Process.Kill()
	}
	return p.file.Close()
}
