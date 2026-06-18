package watcher

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "creds.json")
	want := Credentials{
		Version:         CredentialsVersion,
		HubURL:          "https://hub.example.com",
		GitHubUserID:    12345,
		GitHubUserLogin: "alice",
		Tokens: []TokenRecord{
			{InstallationID: "139674548", Org: "acme", Token: "raw-abc", TokenID: "01HX", DeviceLabel: "laptop", ExpiresAt: 1717000000},
			{InstallationID: "139674612", Org: "bcorp", Token: "raw-xyz", TokenID: "01HY"},
		},
	}
	if err := SaveCredentials(path, want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	got, ok, err := LoadCredentials(path)
	if err != nil || !ok {
		t.Fatalf("LoadCredentials: ok=%v err=%v", ok, err)
	}
	if got.GitHubUserID != 12345 || got.GitHubUserLogin != "alice" || len(got.Tokens) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Tokens[0].DeviceLabel != "laptop" {
		t.Errorf("DeviceLabel = %q, want laptop", got.Tokens[0].DeviceLabel)
	}
}

func TestSaveCredentials_Permissions0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only permission check")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "perm-test.json")
	if err := SaveCredentials(path, Credentials{Version: CredentialsVersion}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 0600", mode)
	}
}

func TestSaveCredentials_AtomicRename(t *testing.T) {
	// Pre-existing file is fully replaced (not appended to) by Save.
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.json")
	if err := os.WriteFile(path, []byte("garbage that is not json"), 0o600); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	want := Credentials{Version: CredentialsVersion, GitHubUserID: 42}
	if err := SaveCredentials(path, want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	got, _, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got.GitHubUserID != 42 {
		t.Errorf("save did not replace garbage; got %+v", got)
	}
	// No leftover tmp files in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestLoadCredentials_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, ok, err := LoadCredentials(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Errorf("missing file errored: %v", err)
	}
	if ok {
		t.Errorf("missing file returned ok=true")
	}
}

func TestLoadCredentials_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v999.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := LoadCredentials(path)
	if err == nil {
		t.Errorf("expected version mismatch error")
	}
}

func TestLoadCredentials_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte(`{not even close to json`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := LoadCredentials(path)
	if err == nil {
		t.Errorf("expected decode error")
	}
}

func TestClearCredentials_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "to-clear.json")
	if err := SaveCredentials(path, Credentials{Version: CredentialsVersion}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ClearCredentials(path); err != nil {
		t.Fatalf("ClearCredentials (first): %v", err)
	}
	if err := ClearCredentials(path); err != nil {
		t.Errorf("ClearCredentials on missing file errored: %v", err)
	}
}

func TestResolveTokenForInstallation(t *testing.T) {
	c := Credentials{Tokens: []TokenRecord{
		{InstallationID: "1", Token: "tok-a"},
		{InstallationID: "2", Token: "tok-b"},
	}}
	if tok, ok := c.ResolveTokenForInstallation("2"); !ok || tok != "tok-b" {
		t.Errorf("got %q ok=%v, want tok-b", tok, ok)
	}
	if _, ok := c.ResolveTokenForInstallation("ghost"); ok {
		t.Errorf("ghost installation matched")
	}
	empty := Credentials{}
	if _, ok := empty.FirstToken(); ok {
		t.Errorf("empty FirstToken returned ok=true")
	}
}
