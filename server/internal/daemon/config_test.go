package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPatternsFromEnv_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("MULTICA_GC_ARTIFACT_PATTERNS", "")
	defaults := []string{"node_modules", ".next", ".turbo"}
	got := patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("expected defaults %v, got %v", defaults, got)
	}
	// Ensure callers get a copy, not a shared backing array.
	got[0] = "mutated"
	if defaults[0] == "mutated" {
		t.Fatal("patternsFromEnv must not return a slice aliased with defaults")
	}
}

func TestPatternsFromEnv_DropsSeparatorBearingEntries(t *testing.T) {
	t.Setenv("MULTICA_GC_ARTIFACT_PATTERNS", "node_modules, .next ,foo/bar, ../etc, ,target")
	got := patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", nil)
	want := []string{"node_modules", ".next", "target"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestIsSafeAgentName(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"claude", true},
		{"cursor-agent", true},
		{"kiro_cli", true},
		{"v1.2", true},
		{"Claude2", true},
		{"", false},
		{"a b", false},
		{"a/b", false},
		{"a;b", false},
		{"a$b", false},
		{"a`b", false},
		{"a'b", false},
		{`a"b`, false},
	} {
		if got := isSafeAgentName(tc.in); got != tc.want {
			t.Errorf("isSafeAgentName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildLoginShellResolveScript_ShapeAndContent(t *testing.T) {
	got := buildLoginShellResolveScript([]string{"claude", "cursor-agent"})
	// Must list exactly the names we asked for, in order.
	if !strings.Contains(got, "for n in claude cursor-agent;") {
		t.Errorf("script missing expected for-loop header:\n%s", got)
	}
	// Must use POSIX `command -v`, not a bash/zsh-specific builtin.
	if !strings.Contains(got, "command -v ") {
		t.Errorf("script missing `command -v` lookup:\n%s", got)
	}
	// Must canonicalise via `cd ... && pwd -P` to break out of symlinked
	// per-shell prefix dirs (fnm/nvm/volta) before the spawned shell exits.
	if !strings.Contains(got, "pwd -P") {
		t.Errorf("script missing pwd -P canonicalisation:\n%s", got)
	}
	// Output must be tab-separated `<name>\t<path>` so the parser can split.
	if !strings.Contains(got, `printf '%s\t%s\n'`) {
		t.Errorf("script missing tab-separated printf:\n%s", got)
	}
}

// TestResolveAgentsViaLoginShell_ResolvesViaInteractiveShell verifies the
// motivating bug scenario: a binary that lives in a directory which is NOT on
// the daemon's PATH but IS added to PATH by the user's interactive shell rc
// file gets resolved to a canonical absolute path.
//
// We simulate this by:
//   - creating a temp dir containing an executable named "fakeclaude"
//   - removing every other dir from PATH (so exec.LookPath misses)
//   - pointing SHELL at /bin/sh and using ENV (sourced on -i) to add the dir
//
// Skipped on Windows (no POSIX shell), and skipped if /bin/sh is missing or
// doesn't honour ENV (which would defeat the simulation — not the function's
// fault).
func TestResolveAgentsViaLoginShell_ResolvesViaInteractiveShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell not available on Windows")
	}
	sh := "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skipf("no /bin/sh available: %v", err)
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "fakeclaude")
	// A trivially executable script. We only need it to exist and be
	// marked +x; the resolver never runs it.
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// Prove the precondition: with binDir absent from PATH, the daemon
	// would normally miss this binary.
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := lookPathInPath("fakeclaude"); err == nil {
		t.Skip("PATH leak — test environment already exposes fakeclaude without shell help")
	}

	// Wire the interactive shell to add binDir to PATH on startup. POSIX
	// sh reads $ENV when invoked with -i, so we write a tiny rc file that
	// prepends binDir.
	rc := filepath.Join(t.TempDir(), "sh.rc")
	if err := os.WriteFile(rc, []byte("export PATH=\""+binDir+":$PATH\"\n"), 0o644); err != nil {
		t.Fatalf("write rc: %v", err)
	}
	t.Setenv("SHELL", sh)
	t.Setenv("ENV", rc)

	got := resolveAgentsViaLoginShell([]string{"fakeclaude", "kiro-cli"})
	resolved, ok := got["fakeclaude"]
	if !ok {
		t.Fatalf("expected fakeclaude in resolved map, got %v", got)
	}
	// Must be an absolute path, must exist, must point at our fake binary
	// (resolving any symlinks t.TempDir may have introduced — macOS's
	// /var → /private/var symlink is the usual culprit).
	if !filepath.IsAbs(resolved) {
		t.Errorf("expected absolute path, got %q", resolved)
	}
	wantCanonical, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		t.Fatalf("eval symlinks for expected path: %v", err)
	}
	if resolved != wantCanonical {
		t.Errorf("resolved = %q, want canonical %q", resolved, wantCanonical)
	}
}

func TestResolveAgentsViaLoginShell_SkipsUnsupportedShell(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	got := resolveAgentsViaLoginShell([]string{"claude"})
	if len(got) != 0 {
		t.Errorf("expected empty map for unsupported shell, got %v", got)
	}
}

func TestResolveAgentsViaLoginShell_EmptyShellNoCrash(t *testing.T) {
	t.Setenv("SHELL", "")
	got := resolveAgentsViaLoginShell([]string{"claude"})
	if len(got) != 0 {
		t.Errorf("expected empty map when SHELL unset, got %v", got)
	}
}

func TestResolveAgentsViaLoginShell_EmptyInput(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	got := resolveAgentsViaLoginShell(nil)
	if len(got) != 0 {
		t.Errorf("expected empty map for nil input, got %v", got)
	}
}

// lookPathInPath is a thin wrapper used by the test above; matches what
// exec.LookPath would do but lets the test be explicit about which call it's
// asserting against.
func lookPathInPath(name string) (string, error) {
	return exec.LookPath(name)
}
