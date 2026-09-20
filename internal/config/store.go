package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

const (
	configDirName  = ".intraflow"
	configFileName = "config.json"
)

// ConfigPath returns the absolute path to the user's config file at
// ~/.intraflow/config.json. It does not create the file or its parent
// directory; callers should ensure the directory exists before writing.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDirName, configFileName), nil
}

// Load reads the config file from disk and returns a parsed Config.
//
// Semantics:
//   - File missing: a default Config is returned with a nil error.
//   - JSON corrupt OR structurally mismatched (e.g. an old-format file with a
//     top-level "records" key, or wrong field types): the mismatch is logged
//     via the standard log package and an EMPTY (default) Config is returned
//     with a nil error. No migration is attempted.
//   - Other I/O errors are returned to the caller.
//
// On a successful parse, Settings.DNSRefreshMinutes is normalized to
// DefaultDNSRefreshMinutes (5) when it is zero (e.g. the key was absent from
// an otherwise-valid file).
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewDefaultConfig(), nil
		}
		return nil, err
	}

	// Detect old-format / structural mismatch before typed unmarshal: the
	// presence of a top-level "records" key indicates a pre-split config and
	// counts as a mismatch per the reset policy. A top-level "domains" or
	// "forwards" key with a wrong type is caught by the typed unmarshal below.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("config: corrupt config at %s (%v); resetting to empty", path, err)
		return NewDefaultConfig(), nil
	}
	if _, ok := raw["records"]; ok {
		log.Printf("config: legacy %q key found at %s; resetting to empty (no migration)", "records", path)
		return NewDefaultConfig(), nil
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("config: structurally mismatched config at %s (%v); resetting to empty", path, err)
		return NewDefaultConfig(), nil
	}

	// Ensure slices are never nil so callers can iterate safely.
	if cfg.Domains == nil {
		cfg.Domains = []Domain{}
	}
	if cfg.Forwards == nil {
		cfg.Forwards = []Forward{}
	}

	// Normalize the DNS refresh default: a zero value (key absent or
	// explicitly 0 in an otherwise-valid file) becomes the default.
	if cfg.Settings.DNSRefreshMinutes == 0 {
		cfg.Settings.DNSRefreshMinutes = DefaultDNSRefreshMinutes
	}

	return &cfg, nil
}

// Save writes the config to ~/.intraflow/config.json as pretty-printed JSON.
//
// The parent directory is created with 0700 permissions if needed. The write
// is atomic: the JSON is first written to a temporary file in the same
// directory and then renamed over the final path. The destination file is
// created with 0600 permissions.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	// Write to a temp file in the same directory so the rename is atomic on
	// the same filesystem.
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}