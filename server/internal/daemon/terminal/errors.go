package terminal

import "errors"

// Sentinel errors returned by Manager. Callers map these to the
// protocol.TerminalErrorCode* constants when reporting to clients.
var (
	ErrTaskNotFound      = errors.New("terminal: task not found")
	ErrWorkspaceMismatch = errors.New("terminal: task belongs to a different workspace")
	ErrSessionNotFound   = errors.New("terminal: session not found")
	ErrUnsupportedOS     = errors.New("terminal: PTY not supported on this OS")
	ErrSpawnFailed       = errors.New("terminal: failed to spawn shell")
	ErrManagerClosed     = errors.New("terminal: manager is shut down")
)
