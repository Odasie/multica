package terminal

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePTY is a Spawner-served stand-in for a real PTY. Tests push child
// output via WriteFromChild and read client input via ReadFromClient.
type fakePTY struct {
	t *testing.T

	// child -> client (output queue, read by readLoop)
	childToClient chan []byte

	// client -> child (writes captured into a buffer slice under mu)
	mu          sync.Mutex
	clientWrote [][]byte
	cols, rows  uint16

	// closeOnce coordinates teardown
	closeOnce sync.Once
	closeCh   chan struct{}

	// waitDone signals Wait can return. Defaults closed by Close.
	waitOnce  sync.Once
	waitDone  chan struct{}
	exitCode  int32
	resizedCh chan [2]uint16
	closed    atomic.Bool
}

func newFakePTY(t *testing.T, cols, rows uint16) *fakePTY {
	return &fakePTY{
		t:             t,
		childToClient: make(chan []byte, 8),
		cols:          cols,
		rows:          rows,
		closeCh:       make(chan struct{}),
		waitDone:      make(chan struct{}),
		resizedCh:     make(chan [2]uint16, 8),
	}
}

func (p *fakePTY) Read(b []byte) (int, error) {
	select {
	case chunk, ok := <-p.childToClient:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, chunk)
		return n, nil
	case <-p.closeCh:
		return 0, io.EOF
	}
}

func (p *fakePTY) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	p.mu.Lock()
	c := make([]byte, len(b))
	copy(c, b)
	p.clientWrote = append(p.clientWrote, c)
	p.mu.Unlock()
	return len(b), nil
}

func (p *fakePTY) Resize(cols, rows uint16) error {
	if p.closed.Load() {
		return io.ErrClosedPipe
	}
	p.mu.Lock()
	p.cols, p.rows = cols, rows
	p.mu.Unlock()
	select {
	case p.resizedCh <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}

func (p *fakePTY) Wait() (int, error) {
	<-p.waitDone
	return int(atomic.LoadInt32(&p.exitCode)), nil
}

func (p *fakePTY) Close() error {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.closeCh)
		close(p.childToClient)
		p.waitOnce.Do(func() { close(p.waitDone) })
	})
	return nil
}

// pushChildOutput simulates the shell writing bytes to its stdout/stderr.
func (p *fakePTY) pushChildOutput(b []byte) {
	select {
	case p.childToClient <- b:
	case <-time.After(time.Second):
		p.t.Fatalf("childToClient send timed out — readLoop not draining")
	}
}

func (p *fakePTY) writes() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.clientWrote))
	copy(out, p.clientWrote)
	return out
}

func (p *fakePTY) size() (uint16, uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// fakeSpawner records each spawn so tests can inspect injected env / cwd.
type fakeSpawner struct {
	t        *testing.T
	spawnsMu sync.Mutex
	spawns   []SpawnRequest
	make     func(*testing.T, SpawnRequest) (*fakePTY, error)
}

func (s *fakeSpawner) Start(req SpawnRequest) (PTY, error) {
	s.spawnsMu.Lock()
	s.spawns = append(s.spawns, req)
	s.spawnsMu.Unlock()
	pty, err := s.make(s.t, req)
	if err != nil {
		return nil, err
	}
	return pty, nil
}

func (s *fakeSpawner) lastRequest() SpawnRequest {
	s.spawnsMu.Lock()
	defer s.spawnsMu.Unlock()
	if len(s.spawns) == 0 {
		return SpawnRequest{}
	}
	return s.spawns[len(s.spawns)-1]
}

// helper: build a Manager with a default fake spawner and a single task.
type fixture struct {
	mgr     *Manager
	spawner *fakeSpawner
	tasks   map[string]TaskInfo
	now     func() time.Time
	clockMu sync.Mutex
	clock   time.Time
}

func newFixture(t *testing.T, opts ...func(*ManagerConfig)) *fixture {
	f := &fixture{
		tasks: map[string]TaskInfo{
			"task-1": {
				TaskID:         "task-1",
				WorkspaceID:    "ws-A",
				IssueID:        "issue-1",
				WorkDir:        t.TempDir(),
				PriorSessionID: "claude-session-xyz",
			},
		},
		clock: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	f.now = func() time.Time {
		f.clockMu.Lock()
		defer f.clockMu.Unlock()
		return f.clock
	}
	f.spawner = &fakeSpawner{
		t:    t,
		make: func(tt *testing.T, req SpawnRequest) (*fakePTY, error) { return newFakePTY(tt, req.Cols, req.Rows), nil },
	}
	cfg := ManagerConfig{
		ShellPath:   "/usr/bin/bash",
		ShellArgs:   []string{"-l"},
		IdleTimeout: 0,
		Spawner:     f.spawner,
		Now:         f.now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	lookup := func(_ context.Context, id string) (TaskInfo, error) {
		info, ok := f.tasks[id]
		if !ok {
			return TaskInfo{}, ErrTaskNotFound
		}
		return info, nil
	}
	f.mgr = NewManager(cfg, lookup)
	return f
}

func (f *fixture) advance(d time.Duration) {
	f.clockMu.Lock()
	f.clock = f.clock.Add(d)
	f.clockMu.Unlock()
}

// drainPTY pulls the *fakePTY back out of the spawner so tests can drive it.
func (f *fixture) lastPTY(t *testing.T) *fakePTY {
	t.Helper()
	req := f.spawner.lastRequest()
	if req.Shell == "" {
		t.Fatal("no spawn recorded")
	}
	// The Spawner.make closure always returns a *fakePTY; the manager
	// wraps it as a PTY interface and we don't retain the concrete in
	// the manager. Re-acquire via the registry by walking sessions.
	for _, id := range f.mgr.Sessions() {
		s, err := f.mgr.Get(id)
		if err == nil {
			if fp, ok := s.pty.(*fakePTY); ok {
				return fp
			}
		}
	}
	t.Fatal("no fake PTY found in any registered session")
	return nil
}

func TestManager_OpenSpawnsWithInjectedEnvAndCwd(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
		UserID:      "user-42",
		Cols:        120,
		Rows:        40,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if sess.ID() == "" {
		t.Fatal("session ID empty")
	}
	if got := sess.WorkDir(); got != f.tasks["task-1"].WorkDir {
		t.Errorf("workdir = %q, want %q", got, f.tasks["task-1"].WorkDir)
	}

	req := f.spawner.lastRequest()
	if req.Cwd != f.tasks["task-1"].WorkDir {
		t.Errorf("spawn cwd = %q, want %q", req.Cwd, f.tasks["task-1"].WorkDir)
	}
	if req.Cols != 120 || req.Rows != 40 {
		t.Errorf("spawn size = %dx%d, want 120x40", req.Cols, req.Rows)
	}

	wantEnv := map[string]string{
		"MULTICA_WORKSPACE_ID": "ws-A",
		"MULTICA_TASK_ID":      "task-1",
		"MULTICA_ISSUE_ID":     "issue-1",
		"MULTICA_USER_ID":      "user-42",
		"CLAUDE_SESSION_ID":    "claude-session-xyz",
	}
	envMap := map[string]string{}
	for _, kv := range req.Env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				envMap[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, want := range wantEnv {
		if got := envMap[k]; got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
}

func TestManager_DefaultSize(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	_, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	req := f.spawner.lastRequest()
	if req.Cols != 80 || req.Rows != 24 {
		t.Errorf("default size = %dx%d, want 80x24", req.Cols, req.Rows)
	}
}

func TestManager_RejectsCrossWorkspace(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	_, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-B-not-the-tasks-workspace",
	})
	if !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("Open err = %v, want ErrWorkspaceMismatch", err)
	}
	if got := len(f.mgr.Sessions()); got != 0 {
		t.Errorf("Sessions after rejected open = %d, want 0", got)
	}
}

func TestManager_RejectsUnknownTask(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	_, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "does-not-exist",
		WorkspaceID: "ws-A",
	})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Open err = %v, want ErrTaskNotFound", err)
	}
}

func TestSession_DataRoundTrip(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pty := f.lastPTY(t)

	// client → child
	if _, err := sess.Write([]byte("ls -al\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// child → client
	pty.pushChildOutput([]byte("total 0\n"))

	select {
	case got := <-sess.Output():
		if string(got) != "total 0\n" {
			t.Errorf("Output chunk = %q, want %q", got, "total 0\n")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Output chunk")
	}

	writes := pty.writes()
	if len(writes) != 1 || string(writes[0]) != "ls -al\n" {
		t.Errorf("recorded writes = %#v, want one 'ls -al\\n'", writes)
	}
}

func TestSession_Resize(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
		Cols:        80, Rows: 24,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pty := f.lastPTY(t)

	if err := sess.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	c, r := sess.Size()
	if c != 132 || r != 50 {
		t.Errorf("Size = %dx%d, want 132x50", c, r)
	}
	gc, gr := pty.size()
	if gc != 132 || gr != 50 {
		t.Errorf("PTY size = %dx%d, want 132x50", gc, gr)
	}
}

func TestSession_CloseDeregistersAndDelivers(t *testing.T) {
	f := newFixture(t)
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := sess.ID()

	sess.Close("user_requested")

	select {
	case info := <-sess.ExitC():
		if info.Reason != "user_requested" {
			t.Errorf("exit reason = %q, want user_requested", info.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ExitC")
	}

	// Output should close once exit fires; verify by ranging.
	drained := false
	for range sess.Output() {
		drained = true
	}
	_ = drained

	<-sess.Done()

	// Session must be deregistered.
	if _, err := f.mgr.Get(id); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get after Close = %v, want ErrSessionNotFound", err)
	}
}

func TestManager_IdleTimeoutSweep(t *testing.T) {
	f := newFixture(t, func(c *ManagerConfig) {
		c.IdleTimeout = 30 * time.Minute
	})
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{
		TaskID:      "task-1",
		WorkspaceID: "ws-A",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 29 minutes — still active.
	f.advance(29 * time.Minute)
	f.mgr.CheckIdle()
	if _, err := f.mgr.Get(sess.ID()); err != nil {
		t.Fatalf("session evicted before idle timeout: %v", err)
	}

	// Cross the threshold.
	f.advance(2 * time.Minute)
	f.mgr.CheckIdle()

	select {
	case info := <-sess.ExitC():
		if info.Reason != "idle_timeout" {
			t.Errorf("exit reason = %q, want idle_timeout", info.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for idle close")
	}

	if _, err := f.mgr.Get(sess.ID()); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("session not deregistered after idle sweep")
	}
}

func TestManager_CloseTearsDownAllSessions(t *testing.T) {
	f := newFixture(t)
	s1, err := f.mgr.Open(context.Background(), OpenParams{TaskID: "task-1", WorkspaceID: "ws-A"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s2, err := f.mgr.Open(context.Background(), OpenParams{TaskID: "task-1", WorkspaceID: "ws-A"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	f.mgr.Close()

	for _, s := range []*PtySession{s1, s2} {
		select {
		case <-s.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("session %s did not tear down", s.ID())
		}
	}
	if got := len(f.mgr.Sessions()); got != 0 {
		t.Errorf("Sessions after Manager.Close = %d, want 0", got)
	}

	// Subsequent opens must be rejected.
	if _, err := f.mgr.Open(context.Background(), OpenParams{TaskID: "task-1", WorkspaceID: "ws-A"}); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Open after Close = %v, want ErrManagerClosed", err)
	}
}

func TestSession_WriteUpdatesLastIO(t *testing.T) {
	f := newFixture(t, func(c *ManagerConfig) {
		c.IdleTimeout = 30 * time.Minute
	})
	defer f.mgr.Close()

	sess, err := f.mgr.Open(context.Background(), OpenParams{TaskID: "task-1", WorkspaceID: "ws-A"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	f.advance(20 * time.Minute)
	if _, err := sess.Write([]byte("echo hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.advance(20 * time.Minute) // total 40min, but 20 min since last IO
	f.mgr.CheckIdle()

	if _, err := f.mgr.Get(sess.ID()); err != nil {
		t.Fatalf("session evicted despite recent write: %v", err)
	}
}
