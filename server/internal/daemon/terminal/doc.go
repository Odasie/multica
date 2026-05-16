// Package terminal manages interactive PTY sessions bound to a task's
// workdir on the local daemon.
//
// A Manager owns the lifecycle of all live PtySessions. Callers (the
// daemonws bridge today, the CLI socket later) translate WebSocket
// terminal.* frames into method calls on Manager, and the Manager
// forwards PTY output back through a per-session Output channel.
//
// Sessions run with the daemon process's identity — there is no
// additional sandbox. This is the same trust boundary as agent runs
// (which are also daemon-spawned child processes). The Manager only
// enforces that the requesting client's workspace matches the task's
// workspace; anything beyond that is the OS's responsibility.
package terminal
