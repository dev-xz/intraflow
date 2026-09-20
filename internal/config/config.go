// Package config provides persistence and validation for IntraFlow's
// domain/forward configuration and application settings.
//
// Configuration is stored as JSON at ~/.intraflow/config.json. The package is
// dependency-free (standard library only) and safe to use from the Wails
// backend as well as from tests.
//
// Model (Lane A of the split-domain-forward-model change):
//   - Domain  : a public domain entry (id + domain string).
//   - Forward : a loopback TCP forward (listen port -> target host:port).
//   - Settings: app-level preferences (Paused, DNSRefreshMinutes).
//
// A Record-style single struct no longer exists; domains and forwards are
// independent lists linked only by reference at the orchestrator layer.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Domain describes a public domain that IntraFlow hijacks in the local hosts
// file. It carries no forwarding information; routing is described separately
// by Forward entries.
type Domain struct {
	// ID is a short, stable identifier generated on creation.
	ID string `json:"id"`
	// Domain is the public domain currently used to reach a service via a
	// public tunnel (e.g. "my-tunnel.example.com").
	Domain string `json:"domain"`
}

// Forward describes a single TCP forwarding rule: IntraFlow listens on
// 127.0.0.1:ListenPort and pipes traffic to TargetHost:TargetPort.
type Forward struct {
	// ID is a short, stable identifier generated on creation.
	ID string `json:"id"`
	// ListenPort is the loopback port IntraFlow listens on.
	ListenPort int `json:"listen_port"`
	// TargetHost is the internal hostname to forward traffic to.
	TargetHost string `json:"target_host"`
	// TargetPort is the port on the target host.
	TargetPort int `json:"target_port"`
	// Enabled indicates whether this forward is currently active.
	Enabled bool `json:"enabled"`
}

// Settings holds user-facing application preferences.
//
// Note: auto-start-on-boot is no longer persisted here. The OS-native login
// item / LaunchAgent / Run-key registration is the single source of truth for
// whether IntraFlow launches at login, and its live state is read on demand
// via the runtime package. Stale `auto_start_on_boot` and
// `auto_enable_all_on_start` keys in older config files are treated as a
// structural mismatch (see Load) and reset to empty rather than migrated.
type Settings struct {
	// Paused suspends all forwarding globally without removing config.
	Paused bool `json:"paused"`
	// DNSRefreshMinutes is the interval between periodic re-resolution of
	// forward targets. Defaults to 5; a zero value present in an otherwise
	// valid config is normalized to 5 on load.
	DNSRefreshMinutes int `json:"dns_refresh_minutes"`
}

// Config is the top-level persisted state for IntraFlow.
type Config struct {
	Domains  []Domain  `json:"domains"`
	Forwards []Forward `json:"forwards"`
	Settings Settings  `json:"settings"`
}

// DefaultDNSRefreshMinutes is the default value for Settings.DNSRefreshMinutes
// when the persisted value is missing or zero.
const DefaultDNSRefreshMinutes = 5

// NewDefaultConfig returns a Config populated with default settings and no
// domains or forwards. DNSRefreshMinutes defaults to
// DefaultDNSRefreshMinutes; auto-start is owned by the OS-native login item
// registration (see the runtime package) and is not part of persisted
// settings.
func NewDefaultConfig() *Config {
	return &Config{
		Domains:  []Domain{},
		Forwards: []Forward{},
		Settings: Settings{
			Paused:            false,
			DNSRefreshMinutes: DefaultDNSRefreshMinutes,
		},
	}
}

// NewID returns a short, random hex identifier suitable for a Domain.ID or
// Forward.ID. It uses crypto/rand and panics only if the system CSPRNG is
// unavailable, which is treated as an unrecoverable environment error.
func NewID() string {
	b := make([]byte, 4) // 4 bytes -> 8 hex chars
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail on supported platforms; fall back to
		// a deterministic-ish value rather than crashing the app.
		return fmt.Sprintf("%08x", b)
	}
	return hex.EncodeToString(b)
}