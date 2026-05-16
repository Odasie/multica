package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeHome points HOME at a temp dir for the duration of the test so the
// real ~/.multica is never touched.
func withFakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // Windows
	return dir
}

func TestDaemonCredentials_RoundTrip(t *testing.T) {
	home := withFakeHome(t)

	store, err := LoadDaemonCredentials("")
	if err != nil {
		t.Fatalf("LoadDaemonCredentials on missing file: %v", err)
	}
	if len(store.Credentials) != 0 {
		t.Fatalf("expected empty store, got %+v", store.Credentials)
	}

	cred := DaemonCredential{
		ServerURL:   "https://api.multica.test/",
		WorkspaceID: "ws-1",
		DaemonID:    "d-1",
		DaemonToken: "mdt_secret",
	}
	store = UpsertDaemonCredential(store, cred)
	if err := SaveDaemonCredentials(store, ""); err != nil {
		t.Fatalf("SaveDaemonCredentials: %v", err)
	}

	// File must exist with 0600 perms.
	path := filepath.Join(home, ".multica", credentialsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file missing: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected mode 0600, got %o", mode)
	}

	// Reload + lookup with the un-normalized URL must find the credential.
	store2, err := LoadDaemonCredentials("")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := FindDaemonCredential(store2, "https://api.multica.test", "d-1", "")
	if !ok {
		t.Fatalf("FindDaemonCredential: not found")
	}
	if got.DaemonToken != cred.DaemonToken {
		t.Fatalf("expected token %q, got %q", cred.DaemonToken, got.DaemonToken)
	}
}

func TestDaemonCredentials_UpsertReplaces(t *testing.T) {
	withFakeHome(t)
	store := DaemonCredentialStore{Version: 1}
	store = UpsertDaemonCredential(store, DaemonCredential{
		ServerURL: "https://api.multica.test", WorkspaceID: "w", DaemonID: "d", DaemonToken: "old",
	})
	store = UpsertDaemonCredential(store, DaemonCredential{
		ServerURL: "https://api.multica.test/", WorkspaceID: "w", DaemonID: "d", DaemonToken: "new",
	})
	if len(store.Credentials) != 1 {
		t.Fatalf("expected single entry after upsert, got %d", len(store.Credentials))
	}
	if store.Credentials[0].DaemonToken != "new" {
		t.Fatalf("expected token 'new', got %q", store.Credentials[0].DaemonToken)
	}
}

func TestDaemonCredentials_Remove(t *testing.T) {
	store := DaemonCredentialStore{Version: 1, Credentials: []DaemonCredential{
		{ServerURL: "https://a", WorkspaceID: "w1", DaemonID: "d", DaemonToken: "t1"},
		{ServerURL: "https://a", WorkspaceID: "w2", DaemonID: "d", DaemonToken: "t2"},
		{ServerURL: "https://b", WorkspaceID: "w1", DaemonID: "d", DaemonToken: "t3"},
	}}
	store = RemoveDaemonCredential(store, "https://a", "d", "w1")
	if len(store.Credentials) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(store.Credentials))
	}
	for _, c := range store.Credentials {
		if c.ServerURL == "https://a" && c.WorkspaceID == "w1" {
			t.Fatalf("entry not removed: %+v", c)
		}
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"https://API.Multica.test/": "https://api.multica.test",
		"https://api.multica.test":  "https://api.multica.test",
		"http://localhost:8080/":    "http://localhost:8080",
		"  http://localhost:8080/ ": "http://localhost:8080",
	}
	for in, want := range cases {
		if got := normalizeServerURL(in); got != want {
			t.Errorf("normalizeServerURL(%q): got %q want %q", in, got, want)
		}
	}
}
