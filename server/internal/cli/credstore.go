package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// credentialsFile is the on-disk store for long-lived daemon credentials
// (`mdt_` tokens issued via POST /api/install-tokens/exchange). It is the
// "daemon keychain" the RFC v6.1 §6.4 / R1 calls for: a per-machine credential
// store independent of the user-facing CLI config (`mul_` PAT).
//
// File location:
//
//   - Default profile: ~/.multica/credentials.json
//   - Named profile:   ~/.multica/profiles/<name>/credentials.json
//
// File mode is 0600 (owner read/write only); the parent directory is 0700.
// The file is rewritten atomically (write-temp + rename).
const credentialsFile = "credentials.json"

// DaemonCredential is a single (server, workspace) credential entry. A daemon
// installed across multiple workspaces holds one entry per server-base-URL +
// workspace pair: re-running `multica daemon start --install-token <mit_>` for
// a second workspace appends a new row instead of overwriting the first.
type DaemonCredential struct {
	ServerURL   string `json:"server_url"`
	WorkspaceID string `json:"workspace_id"`
	DaemonID    string `json:"daemon_id"`
	DaemonToken string `json:"daemon_token"`
	IssuedAt    string `json:"issued_at,omitempty"` // RFC3339; advisory only
}

// DaemonCredentialStore is the parsed contents of credentials.json.
type DaemonCredentialStore struct {
	// Schema version. Bumped only when the on-disk layout changes in a way
	// that older CLIs would silently misread.
	Version     int                `json:"version"`
	Credentials []DaemonCredential `json:"credentials,omitempty"`
}

// DaemonCredentialsPath returns the path to the daemon credentials file for
// the given profile. Empty profile -> ~/.multica/credentials.json.
func DaemonCredentialsPath(profile string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve credentials path: %w", err)
	}
	if profile == "" {
		return filepath.Join(home, ".multica", credentialsFile), nil
	}
	return filepath.Join(home, ".multica", "profiles", profile, credentialsFile), nil
}

// LoadDaemonCredentials reads the credentials store from disk. A missing file
// is not an error and returns an empty store; the caller distinguishes
// "no credential found" via FindDaemonCredential.
func LoadDaemonCredentials(profile string) (DaemonCredentialStore, error) {
	path, err := DaemonCredentialsPath(profile)
	if err != nil {
		return DaemonCredentialStore{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DaemonCredentialStore{Version: 1}, nil
		}
		return DaemonCredentialStore{}, fmt.Errorf("read credentials: %w", err)
	}
	var store DaemonCredentialStore
	if err := json.Unmarshal(data, &store); err != nil {
		return DaemonCredentialStore{}, fmt.Errorf("parse credentials: %w", err)
	}
	if store.Version == 0 {
		store.Version = 1
	}
	return store, nil
}

// SaveDaemonCredentials writes the credentials store to disk atomically with
// 0600 perms. The parent directory is created with 0700.
func SaveDaemonCredentials(store DaemonCredentialStore, profile string) error {
	if store.Version == 0 {
		store.Version = 1
	}
	// Deterministic ordering on disk — helps diff'ing and avoids spurious
	// rewrites when callers reload then save without mutating.
	sort.SliceStable(store.Credentials, func(i, j int) bool {
		a, b := store.Credentials[i], store.Credentials[j]
		if a.ServerURL != b.ServerURL {
			return a.ServerURL < b.ServerURL
		}
		return a.WorkspaceID < b.WorkspaceID
	})

	path, err := DaemonCredentialsPath(profile)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".credentials-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp credentials file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp credentials file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp credentials file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp credentials file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename credentials file: %w", err)
	}
	return nil
}

// FindDaemonCredential returns the credential matching the (server, daemon)
// pair, if any. Match is by normalized server URL — trailing slashes and
// scheme are normalized so a credential issued via `https://api.foo/` is
// returned for a daemon configured with `https://api.foo`.
//
// When workspaceID is non-empty, the match is additionally constrained to
// that workspace. Otherwise the first credential bound to (server, daemon)
// is returned.
func FindDaemonCredential(store DaemonCredentialStore, serverURL, daemonID, workspaceID string) (DaemonCredential, bool) {
	wantServer := normalizeServerURL(serverURL)
	for _, c := range store.Credentials {
		if normalizeServerURL(c.ServerURL) != wantServer {
			continue
		}
		if daemonID != "" && c.DaemonID != daemonID {
			continue
		}
		if workspaceID != "" && c.WorkspaceID != workspaceID {
			continue
		}
		return c, true
	}
	return DaemonCredential{}, false
}

// UpsertDaemonCredential inserts or replaces a credential keyed on
// (server, workspace, daemon). Two installs against the same trio collapse
// to a single row — the latest mdt_ wins, which matches the server-side
// behaviour where a fresh exchange always issues a new daemon_token.
func UpsertDaemonCredential(store DaemonCredentialStore, cred DaemonCredential) DaemonCredentialStore {
	want := normalizeServerURL(cred.ServerURL)
	for i, c := range store.Credentials {
		if normalizeServerURL(c.ServerURL) == want && c.WorkspaceID == cred.WorkspaceID && c.DaemonID == cred.DaemonID {
			store.Credentials[i] = cred
			return store
		}
	}
	store.Credentials = append(store.Credentials, cred)
	return store
}

// RemoveDaemonCredential drops any credential matching the (server, daemon)
// pair. Used by `multica daemon stop --logout` style flows (future) and by
// the Remove Computer path when the daemon receives a 401-revoked response.
func RemoveDaemonCredential(store DaemonCredentialStore, serverURL, daemonID, workspaceID string) DaemonCredentialStore {
	want := normalizeServerURL(serverURL)
	out := store.Credentials[:0]
	for _, c := range store.Credentials {
		if normalizeServerURL(c.ServerURL) == want && c.DaemonID == daemonID && (workspaceID == "" || c.WorkspaceID == workspaceID) {
			continue
		}
		out = append(out, c)
	}
	store.Credentials = out
	return store
}

// normalizeServerURL strips trailing slashes and normalizes scheme casing so
// equivalent URLs collapse to one key. Mirrors daemon.NormalizeServerBaseURL
// without importing the daemon package (avoid CLI ↔ daemon import cycle).
func normalizeServerURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	return strings.TrimRight(s, "/")
}
