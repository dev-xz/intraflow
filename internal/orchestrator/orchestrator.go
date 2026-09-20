// Package orchestrator ties together IntraFlow's three backend packages —
// config, forwarder, and hosts — into a single coordination layer that the
// IPC host and Wails frontend interact with.
//
// An Orchestrator owns the application Config, a forwarder Pool, and a context.
// It is safe for concurrent use: every method that mutates config or the pool
// holds the orchestrator's mutex. Hosts-file writes are NOT performed by the
// orchestrator; instead every mutating flow is split into a Prepare step
// (validates, builds hosts content, no elevation) and a Complete step
// (finalizes config + reconciles the forwarder pool). The GUI process performs
// the elevated hosts write between the two calls. The readHosts/writeHosts
// hooks are injected for tests so they can supply canned hosts content without
// touching the real /etc/hosts.
//
// Lane C of the OpenSpec split-domain-forward-model change: domains and
// forwards are independent lists. A forward runs iff
// !settings.Paused && forward.Enabled. The hosts zone contains one
// "127.0.0.1 <domain>" entry per Domain (empty when paused).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"intraflow/internal/config"
	"intraflow/internal/forwarder"
	"intraflow/internal/hosts"
)

// Orchestrator coordinates config, hosts, and forwarders for IntraFlow.
//
// All exported methods are safe to call concurrently. Methods that mutate
// config state do so under mu; the forwarder Pool is itself concurrency-safe.
type Orchestrator struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	config *config.Config
	pool   *forwarder.Pool

	// Injectable hooks for tests. In production these default to the real
	// config/hosts operations; tests replace them via NewForTesting.
	loadConfig func() (*config.Config, error)
	saveConfig func(*config.Config) error
	readHosts  func() (string, error)
	writeHosts func(content string) error // retained for signature compat; unused in the split model
}

// New loads the real config from disk, creates a forwarder Pool, and returns
// an Orchestrator ready for use. It does NOT run StartupCheck; the caller
// (typically the host entry point) invokes StartupCheck separately.
func New(ctx context.Context) (*Orchestrator, error) {
	innerCtx, cancel := context.WithCancel(ctx)
	cfg, err := config.Load()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("orchestrator: load config: %w", err)
	}
	return &Orchestrator{
		ctx:        innerCtx,
		cancel:     cancel,
		config:     cfg,
		pool:       forwarder.NewPool(),
		loadConfig: config.Load,
		saveConfig: func(c *config.Config) error { return c.Save() },
		readHosts:  defaultReadHosts,
		writeHosts: hosts.WriteHostsElevated,
	}, nil
}

// NewForTesting constructs an Orchestrator that bypasses disk entirely. It is
// intended only for tests: cfg is used as the in-memory config, and the
// readHosts/writeHosts hooks let tests inject canned hosts content and capture
// (or refuse) writes without triggering a real elevation prompt. The
// writeHosts hook is retained for signature compatibility but is not called
// by any method in the split Prepare/Complete model.
func NewForTesting(ctx context.Context, cfg *config.Config, readHosts func() (string, error), writeHosts func(string) error) *Orchestrator {
	innerCtx, cancel := context.WithCancel(ctx)
	return &Orchestrator{
		ctx:        innerCtx,
		cancel:     cancel,
		config:     cfg,
		pool:       forwarder.NewPool(),
		loadConfig: func() (*config.Config, error) { return cfg, nil },
		saveConfig: func(*config.Config) error { return nil },
		readHosts:  readHosts,
		writeHosts: writeHosts,
	}
}

// defaultReadHosts reads the system hosts file. A missing file is treated as
// empty content so a fresh install with no hosts zone behaves correctly.
func defaultReadHosts() (string, error) {
	b, err := os.ReadFile(hosts.HostsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// ListDomains returns a snapshot copy of the config's domain list, preserving
// insertion order. The returned slice is safe to mutate without affecting the
// orchestrator's state.
func (o *Orchestrator) ListDomains() []config.Domain {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]config.Domain, len(o.config.Domains))
	copy(out, o.config.Domains)
	return out
}

// ListForwards returns a snapshot copy of the config's forward list,
// preserving insertion order. The returned slice is safe to mutate without
// affecting the orchestrator's state.
func (o *Orchestrator) ListForwards() []config.Forward {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]config.Forward, len(o.config.Forwards))
	copy(out, o.config.Forwards)
	return out
}

// GetSettings returns the current application settings.
func (o *Orchestrator) GetSettings() config.Settings {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.config.Settings
}

// SetSettings updates the application settings and persists them to disk.
// DNSRefreshMinutes is clamped to config.DefaultDNSRefreshMinutes when it is
// less than 1. The new refresh interval applies to forwards on their next
// start (no live reticker is adjusted for already-running forwards).
func (o *Orchestrator) SetSettings(s config.Settings) error {
	if s.DNSRefreshMinutes < 1 {
		s.DNSRefreshMinutes = config.DefaultDNSRefreshMinutes
	}
	o.mu.Lock()
	o.config.Settings = s
	cfg := o.config
	o.mu.Unlock()
	return o.saveConfig(cfg)
}

// Statuses returns a snapshot of every forwarder's status in the pool, keyed
// by forward ID.
func (o *Orchestrator) Statuses() map[string]forwarder.StatusInfo {
	return o.pool.Statuses()
}

// StopAll stops every running forwarder. Intended for the app-quit path.
func (o *Orchestrator) StopAll() {
	o.pool.StopAll()
}

// CurrentHostsZone reads the hosts file and returns the text of the IntraFlow
// marker zone (the lines between BeginMarker and EndMarker, inclusive of the
// markers). Returns "" when no zone is present.
func (o *Orchestrator) CurrentHostsZone() (string, error) {
	content, err := o.readHosts()
	if err != nil {
		return "", err
	}
	_, entries, _, hasZone := hosts.ParseZone(content)
	if !hasZone {
		return "", nil
	}
	return hosts.BuildZone(entries), nil
}

// StartupCheck compares the expected hosts zone (derived from the effective
// state: paused → empty, otherwise one entry per Domain) against the current
// hosts file and, when consistent, silently starts forwards for every
// effective forward (none when paused). It never elevates: an inconsistent
// state is returned as a Diff for the GUI to surface a "Fix now" banner (see
// PrepareFixNow / CompleteFixNow).
//
// When the diff is NOT consistent, no forwards are started — even for
// forwards whose domains happen to be present in hosts. This is a deliberate
// safety choice: a partially-consistent state likely indicates external
// tampering or a crashed previous run, and auto-starting a subset could
// surprise the user with half-working takeovers. FixNow resolves everything
// in one click. A paused state with a non-empty zone is inconsistent (the
// fix is an empty zone).
func (o *Orchestrator) StartupCheck() (hosts.Diff, error) {
	o.mu.Lock()
	paused := o.config.Settings.Paused
	refreshMin := o.config.Settings.DNSRefreshMinutes
	var effectiveForwards []config.Forward
	for _, f := range o.config.Forwards {
		if !paused && f.Enabled {
			effectiveForwards = append(effectiveForwards, f)
		}
	}
	expectedDomains := o.expectedDomainsLocked()
	o.mu.Unlock()

	content, err := o.readHosts()
	if err != nil {
		return hosts.Diff{}, fmt.Errorf("read hosts: %w", err)
	}
	diff := hosts.CheckConsistency(expectedDomains, content)

	if diff.Consistent {
		for _, f := range effectiveForwards {
			if err := o.pool.StartWithRefresh(o.ctx, f.ID, f.ListenPort, f.TargetHost, f.TargetPort, time.Duration(refreshMin)*time.Minute, nil); err != nil {
				// A single failure to start shouldn't abort the whole
				// startup; the user can retry via FixNow. The failed
				// forwarder remains in the pool in StatusError.
				_ = err
			}
		}
	}
	return diff, nil
}

// PrepareApplyAll validates the full desired domain and forward sets and
// builds the new hosts file content WITHOUT writing it. Returns the new
// content, whether it differs from the current hosts file (so the GUI can
// skip osascript when the change doesn't affect the hosts zone), and the
// domains and forwards with IDs assigned (entries with empty IDs get a fresh
// config.NewID()). No elevation happens here.
//
// Validation: config.ValidateDomains and config.ValidateForwards are run on
// the assigned sets; the first failure is returned unwrapped.
//
// While paused: the generated zone is empty (no entries). If the current
// hosts zone is already empty, changed is false and the GUI skips the
// elevated write. The forward set is still validated and assigned so
// CompleteApplyAll can persist it.
func (o *Orchestrator) PrepareApplyAll(domains []config.Domain, forwards []config.Forward) (newContent string, changed bool, assignedDomains []config.Domain, assignedForwards []config.Forward, err error) {
	// Assign IDs to entries with empty IDs.
	assignedDomains = make([]config.Domain, len(domains))
	copy(assignedDomains, domains)
	for i := range assignedDomains {
		if assignedDomains[i].ID == "" {
			assignedDomains[i].ID = config.NewID()
		}
	}
	assignedForwards = make([]config.Forward, len(forwards))
	copy(assignedForwards, forwards)
	for i := range assignedForwards {
		if assignedForwards[i].ID == "" {
			assignedForwards[i].ID = config.NewID()
		}
	}

	// Validate both lists.
	if err := config.ValidateDomains(assignedDomains); err != nil {
		return "", false, nil, nil, err
	}
	if err := config.ValidateForwards(assignedForwards); err != nil {
		return "", false, nil, nil, err
	}

	// Build the new hosts content from the effective state.
	content, rerr := o.readHosts()
	if rerr != nil {
		return "", false, nil, nil, fmt.Errorf("read hosts: %w", rerr)
	}
	o.mu.Lock()
	paused := o.config.Settings.Paused
	o.mu.Unlock()

	entries := effectiveZoneEntries(paused, assignedDomains)
	newContent = hosts.ReplaceZone(content, entries)
	changed = newContent != content
	return newContent, changed, assignedDomains, assignedForwards, nil
}

// CompleteApplyAll reconciles config to exactly the given sets and syncs the
// forwarder pool to match the final effective state. Called by the GUI after
// the elevated hosts write succeeded (or when no write was needed, i.e. the
// hosts zone was unchanged).
//
// The domains and forwards slices MUST be the assigned sets returned by
// PrepareApplyAll (IDs already populated). The orchestrator:
//  1. Replaces config.Domains and config.Forwards entirely with the incoming
//     sets and persists.
//  2. Reconciles the pool: stops forwards no longer effective (removed,
//     disabled, or paused) and starts (or restarts) each effective forward
//     via StartWithRefresh with the configured DNS refresh interval.
//
// While paused: config is still persisted, but no forwards are started (the
// effective set is empty) and all running forwards are stopped.
func (o *Orchestrator) CompleteApplyAll(domains []config.Domain, forwards []config.Forward) error {
	o.mu.Lock()
	paused := o.config.Settings.Paused
	refreshMin := o.config.Settings.DNSRefreshMinutes
	o.config.Domains = make([]config.Domain, len(domains))
	copy(o.config.Domains, domains)
	o.config.Forwards = make([]config.Forward, len(forwards))
	copy(o.config.Forwards, forwards)
	cfg := o.config
	effective := effectiveForwards(paused, forwards)
	o.mu.Unlock()

	if err := o.saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	o.reconcilePool(effective, refreshMin)
	return nil
}

// PreparePause builds the hosts content with an empty zone — WITHOUT writing.
// Returns the new content and whether it differs from the current hosts file.
// changed is true iff the current zone contains at least one entry (a
// non-existent or already-empty zone means no write is needed).
func (o *Orchestrator) PreparePause() (string, bool, error) {
	content, err := o.readHosts()
	if err != nil {
		return "", false, fmt.Errorf("read hosts: %w", err)
	}
	_, entries, _, hasZone := hosts.ParseZone(content)
	if !hasZone || len(entries) == 0 {
		// Already effectively paused: no write needed.
		return content, false, nil
	}
	newContent := hosts.ReplaceZone(content, nil)
	return newContent, true, nil
}

// CompletePause finalizes a pause after the GUI's elevated write succeeded
// (or was skipped): sets Paused=true, persists config, and stops all
// forwarders.
func (o *Orchestrator) CompletePause() error {
	o.mu.Lock()
	o.config.Settings.Paused = true
	cfg := o.config
	o.mu.Unlock()

	if err := o.saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	o.pool.StopAll()
	return nil
}

// PrepareResume builds the hosts content with one entry per Domain — WITHOUT
// writing. Returns the new content and whether it differs from the current
// hosts file.
func (o *Orchestrator) PrepareResume() (string, bool, error) {
	content, err := o.readHosts()
	if err != nil {
		return "", false, fmt.Errorf("read hosts: %w", err)
	}
	o.mu.Lock()
	// Resume builds the zone as if already active (one entry per Domain),
	// regardless of the current Paused flag — Paused is only flipped in
	// CompleteResume.
	entries := make([]hosts.Entry, 0, len(o.config.Domains))
	for _, d := range o.config.Domains {
		entries = append(entries, hosts.Entry{IP: "127.0.0.1", Host: d.Domain})
	}
	o.mu.Unlock()

	newContent := hosts.ReplaceZone(content, entries)
	changed := newContent != content
	return newContent, changed, nil
}

// CompleteResume finalizes a resume after the GUI's elevated write succeeded
// (or was skipped): sets Paused=false, persists config, and starts every
// effective forward (all Enabled forwards, now that Paused is false).
func (o *Orchestrator) CompleteResume() error {
	o.mu.Lock()
	o.config.Settings.Paused = false
	refreshMin := o.config.Settings.DNSRefreshMinutes
	cfg := o.config
	effective := effectiveForwards(false, o.config.Forwards)
	o.mu.Unlock()

	if err := o.saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	o.reconcilePool(effective, refreshMin)
	return nil
}

// PrepareFixNow rebuilds the hosts zone to match the effective state (paused →
// empty, otherwise one entry per Domain) — WITHOUT writing. Returns the new
// content and whether it differs from the current hosts file. This is the
// user-initiated counterpart to a non-consistent StartupCheck result.
func (o *Orchestrator) PrepareFixNow() (string, bool, error) {
	content, err := o.readHosts()
	if err != nil {
		return "", false, fmt.Errorf("read hosts: %w", err)
	}
	o.mu.Lock()
	entries := o.expectedEntriesLocked()
	o.mu.Unlock()

	newContent := hosts.ReplaceZone(content, entries)
	changed := newContent != content
	return newContent, changed, nil
}

// CompleteFixNow reconciles the forwarder pool to the effective state. Config
// is NOT persisted (the config is unchanged — FixNow only repairs the hosts
// file and pool to match the existing config). Effective forwards are
// started; non-effective forwards are stopped.
func (o *Orchestrator) CompleteFixNow() error {
	o.mu.Lock()
	paused := o.config.Settings.Paused
	refreshMin := o.config.Settings.DNSRefreshMinutes
	effective := effectiveForwards(paused, o.config.Forwards)
	o.mu.Unlock()
	o.reconcilePool(effective, refreshMin)
	return nil
}

// --- helpers ---

// expectedDomainsLocked returns the domain strings that should appear in the
// hosts zone: empty when paused, otherwise one per Domain. Caller must hold
// o.mu.
func (o *Orchestrator) expectedDomainsLocked() []string {
	if o.config.Settings.Paused {
		return nil
	}
	out := make([]string, 0, len(o.config.Domains))
	for _, d := range o.config.Domains {
		out = append(out, d.Domain)
	}
	return out
}

// expectedEntriesLocked returns the hosts.Entry list for the current config's
// effective state: nil when paused, otherwise one "127.0.0.1 <domain>" entry
// per Domain. Caller must hold o.mu.
func (o *Orchestrator) expectedEntriesLocked() []hosts.Entry {
	if o.config.Settings.Paused {
		return nil
	}
	entries := make([]hosts.Entry, 0, len(o.config.Domains))
	for _, d := range o.config.Domains {
		entries = append(entries, hosts.Entry{IP: "127.0.0.1", Host: d.Domain})
	}
	return entries
}

// effectiveZoneEntries returns the hosts.Entry list for the given paused flag
// and domain list. Returns nil when paused.
func effectiveZoneEntries(paused bool, domains []config.Domain) []hosts.Entry {
	if paused {
		return nil
	}
	entries := make([]hosts.Entry, 0, len(domains))
	for _, d := range domains {
		entries = append(entries, hosts.Entry{IP: "127.0.0.1", Host: d.Domain})
	}
	return entries
}

// effectiveForwards returns the forwards that should be running: those that
// are Enabled and not blocked by the paused flag.
func effectiveForwards(paused bool, forwards []config.Forward) []config.Forward {
	if paused {
		return nil
	}
	out := make([]config.Forward, 0, len(forwards))
	for _, f := range forwards {
		if f.Enabled {
			out = append(out, f)
		}
	}
	return out
}

// reconcilePool stops every forwarder in the pool that is not in the desired
// effective set, then starts (or restarts) each desired forward with the
// given DNS refresh interval. A single bind failure does not abort the
// reconcile; the failed forwarder stays in the pool in StatusError.
func (o *Orchestrator) reconcilePool(desired []config.Forward, refreshMinutes int) {
	desiredIDs := make(map[string]struct{}, len(desired))
	for _, f := range desired {
		desiredIDs[f.ID] = struct{}{}
	}
	// Stop forwards no longer effective.
	for id := range o.pool.Statuses() {
		if _, ok := desiredIDs[id]; !ok {
			o.pool.Stop(id)
		}
	}
	// Start/restart each effective forward.
	refreshEvery := time.Duration(refreshMinutes) * time.Minute
	for _, f := range desired {
		if err := o.pool.StartWithRefresh(o.ctx, f.ID, f.ListenPort, f.TargetHost, f.TargetPort, refreshEvery, nil); err != nil {
			// Surface via the forwarder's StatusError state; don't abort
			// the whole reconcile for a single bind failure.
			_ = err
		}
	}
}