package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withTempHome sets HOME to a temp dir for the test and returns the path to
// the config file inside it.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	return path
}

func TestNewDefaultConfig(t *testing.T) {
	c := NewDefaultConfig()
	if c == nil {
		t.Fatal("expected non-nil config")
	}
	if c.Domains == nil {
		t.Fatal("expected non-nil Domains slice")
	}
	if len(c.Domains) != 0 {
		t.Fatalf("expected 0 domains, got %d", len(c.Domains))
	}
	if c.Forwards == nil {
		t.Fatal("expected non-nil Forwards slice")
	}
	if len(c.Forwards) != 0 {
		t.Fatalf("expected 0 forwards, got %d", len(c.Forwards))
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("DNSRefreshMinutes should default to %d, got %d", DefaultDNSRefreshMinutes, c.Settings.DNSRefreshMinutes)
	}
	if c.Settings.Paused {
		t.Error("Paused should default to false")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempHome(t)

	in := NewDefaultConfig()
	in.Domains = []Domain{
		{ID: "abc12345", Domain: "tunnel.example.com"},
		{ID: "deadbeef", Domain: "other.example.com"},
	}
	in.Forwards = []Forward{
		{
			ID:         "fwd00001",
			ListenPort: 8080,
			TargetHost: "internal.svc",
			TargetPort: 80,
			Enabled:    true,
		},
	}
	in.Settings.Paused = true
	in.Settings.DNSRefreshMinutes = 10

	if err := in.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(out.Domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(out.Domains))
	}
	d := out.Domains[0]
	if d.ID != "abc12345" || d.Domain != "tunnel.example.com" {
		t.Fatalf("domain round-trip mismatch: %+v", d)
	}
	if len(out.Forwards) != 1 {
		t.Fatalf("expected 1 forward, got %d", len(out.Forwards))
	}
	f := out.Forwards[0]
	if f.ID != "fwd00001" ||
		f.ListenPort != 8080 ||
		f.TargetHost != "internal.svc" ||
		f.TargetPort != 80 ||
		f.Enabled != true {
		t.Fatalf("forward round-trip mismatch: %+v", f)
	}
	if out.Settings.Paused != in.Settings.Paused {
		t.Fatalf("settings Paused mismatch: %+v", out.Settings)
	}
	if out.Settings.DNSRefreshMinutes != in.Settings.DNSRefreshMinutes {
		t.Fatalf("settings DNSRefreshMinutes mismatch: %+v", out.Settings)
	}
}

// TestLoadNormalizesMissingDNSRefreshMinutes verifies that an otherwise-valid
// config file missing the dns_refresh_minutes key (or with it set to 0) is
// normalized to DefaultDNSRefreshMinutes on load.
func TestLoadNormalizesMissingDNSRefreshMinutes(t *testing.T) {
	path := withTempHome(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	missing := []byte(`{
  "domains": [{"id": "abc12345", "domain": "tunnel.example.com"}],
  "forwards": [],
  "settings": {"paused": false}
}`)
	if err := os.WriteFile(path, missing, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("expected DNSRefreshMinutes=%d, got %d", DefaultDNSRefreshMinutes, c.Settings.DNSRefreshMinutes)
	}

	// Explicit zero should also normalize to default.
	zero := []byte(`{
  "domains": [],
  "forwards": [],
  "settings": {"paused": true, "dns_refresh_minutes": 0}
}`)
	if err := os.WriteFile(path, zero, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("expected DNSRefreshMinutes=%d for explicit zero, got %d", DefaultDNSRefreshMinutes, c.Settings.DNSRefreshMinutes)
	}
	if !c.Settings.Paused {
		t.Fatalf("Paused should round-trip as true")
	}
}

func TestLoadMissingFileReturnsDefault(t *testing.T) {
	withTempHome(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil default config")
	}
	if len(c.Domains) != 0 {
		t.Fatalf("expected no domains, got %d", len(c.Domains))
	}
	if len(c.Forwards) != 0 {
		t.Fatalf("expected no forwards, got %d", len(c.Forwards))
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("expected default DNSRefreshMinutes, got %d", c.Settings.DNSRefreshMinutes)
	}
}

func TestLoadCorruptJSONReturnsEmpty(t *testing.T) {
	path := withTempHome(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{ this is not valid json "), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load on corrupt file should not error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil empty config")
	}
	if len(c.Domains) != 0 || len(c.Forwards) != 0 {
		t.Fatalf("expected empty slices from corrupt file, got domains=%d forwards=%d", len(c.Domains), len(c.Forwards))
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("expected default DNSRefreshMinutes from corrupt file, got %d", c.Settings.DNSRefreshMinutes)
	}
}

// TestLoadOldFormatRecordsKeyReturnsEmpty verifies that a pre-split config
// file containing a top-level "records" key is treated as a structural
// mismatch and reset to empty without migration, per the reset policy.
func TestLoadOldFormatRecordsKeyReturnsEmpty(t *testing.T) {
	path := withTempHome(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	old := []byte(`{
  "records": [
    {
      "id": "abc12345",
      "public_domain": "tunnel.example.com",
      "public_port": 443,
      "local_port": 8080,
      "internal_host": "internal.svc",
      "internal_port": 80,
      "enabled": true
    }
  ],
  "settings": {"auto_enable_all_on_start": true}
}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load on old-format file should not error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil empty config")
	}
	if len(c.Domains) != 0 || len(c.Forwards) != 0 {
		t.Fatalf("expected empty slices from old-format file, got domains=%d forwards=%d", len(c.Domains), len(c.Forwards))
	}
	if c.Settings.DNSRefreshMinutes != DefaultDNSRefreshMinutes {
		t.Fatalf("expected default DNSRefreshMinutes from old-format file, got %d", c.Settings.DNSRefreshMinutes)
	}
}

// TestLoadWrongFieldTypeReturnsEmpty verifies that a structurally mismatched
// config (e.g. "forwards" is a string instead of an array) is treated as a
// mismatch and reset to empty.
func TestLoadWrongFieldTypeReturnsEmpty(t *testing.T) {
	path := withTempHome(t)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bad := []byte(`{
  "domains": [],
  "forwards": "not-an-array",
  "settings": {"paused": false, "dns_refresh_minutes": 5}
}`)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load on mismatched file should not error: %v", err)
	}
	if len(c.Domains) != 0 || len(c.Forwards) != 0 {
		t.Fatalf("expected empty slices from mismatched file, got domains=%d forwards=%d", len(c.Domains), len(c.Forwards))
	}
}

func TestSaveAtomicCreatesValidFile(t *testing.T) {
	path := withTempHome(t)

	c := NewDefaultConfig()
	c.Domains = []Domain{{ID: "deadbeef", Domain: "x.example.com"}}
	c.Forwards = []Forward{
		{
			ID:         "fwd00001",
			ListenPort: 2,
			TargetHost: "y.svc",
			TargetPort: 3,
			Enabled:    false,
		},
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file should exist after Save: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 perms, got %v", info.Mode().Perm())
	}

	// No leftover temp files.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed Config
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("saved file does not parse: %v", err)
	}
	if len(parsed.Domains) != 1 || parsed.Domains[0].ID != "deadbeef" {
		t.Fatalf("parsed domain content mismatch: %+v", parsed)
	}
	if len(parsed.Forwards) != 1 || parsed.Forwards[0].ID != "fwd00001" {
		t.Fatalf("parsed forward content mismatch: %+v", parsed)
	}
}