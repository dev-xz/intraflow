package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"intraflow/internal/config"
	"intraflow/internal/forwarder"
	"intraflow/internal/hosts"
)

// testPortBase returns a per-test stable base port derived from the test name,
// so parallel tests don't collide on SuggestFreePort starting points.
func testPortBase(t *testing.T) int {
	t.Helper()
	var h uint32
	name := t.Name()
	for i := 0; i < len(name); i++ {
		h = h*31 + uint32(name[i])
	}
	return 20000 + int(h%39000)
}

// freeForwardAt builds a valid, enabled Forward with a free ListenPort. The
// offset shifts the port-search range so a single test can request several
// non-overlapping forwards.
func freeForwardAt(t *testing.T, offset int, target string) config.Forward {
	t.Helper()
	port := forwarder.SuggestFreePort(testPortBase(t) + offset)
	if port == 0 {
		t.Fatalf("no free port")
	}
	return config.Forward{
		ID:         config.NewID(),
		ListenPort: port,
		TargetHost: target,
		TargetPort: 8080,
		Enabled:    true,
	}
}

func freeForward(t *testing.T, target string) config.Forward {
	return freeForwardAt(t, 0, target)
}

// fakesHosts returns readHosts/writeHosts fakes. writeHosts records the last
// content written to *written and returns nil.
func fakesHosts(initialContent string, written *string) (func() (string, error), func(string) error) {
	cur := initialContent
	read := func() (string, error) { return cur, nil }
	write := func(content string) error {
		cur = content
		if written != nil {
			*written = content
		}
		return nil
	}
	return read, write
}

func newOrch(t *testing.T, cfg *config.Config, initialHosts string, written *string) *Orchestrator {
	t.Helper()
	// Use a mutable holder so readHosts reflects subsequent writes (mirrors
	// real disk behavior the GUI expects when it re-reads after writing).
	holder := initialHosts
	read := func() (string, error) { return holder, nil }
	write := func(content string) error {
		holder = content
		if written != nil {
			*written = content
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewForTesting(ctx, cfg, read, write)
}

// waitForListening polls the pool for up to 1s waiting for id to be Listening.
func waitForListening(t *testing.T, o *Orchestrator, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := o.pool.Statuses()[id]
		if st.State == forwarder.StatusListening {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("forwarder %s never reached listening", id)
}

// itoa avoids importing strconv just for this.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// --- PrepareApplyAll ---

func TestPrepareApplyAll_Success(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	var written string
	o := newOrch(t, cfg, "", &written)

	f1 := freeForwardAt(t, 0, "127.0.0.1")
	f2 := freeForwardAt(t, 100, "10.0.0.1")
	f2.Enabled = false

	d1 := config.Domain{ID: "", Domain: "aa.example.com"}
	d2 := config.Domain{ID: "d2", Domain: "bb.example.com"}

	content, changed, ad, af, err := o.PrepareApplyAll([]config.Domain{d1, d2}, []config.Forward{f1, f2})
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for empty initial hosts")
	}
	if !strings.Contains(content, "127.0.0.1 aa.example.com") || !strings.Contains(content, "127.0.0.1 bb.example.com") {
		t.Fatalf("content missing domain(s):\n%s", content)
	}
	if len(ad) != 2 || ad[0].ID == "" || ad[1].ID != "d2" {
		t.Fatalf("assigned domains wrong: %+v", ad)
	}
	if len(af) != 2 || af[0].ID == "" || af[1].ID == "" {
		t.Fatalf("assigned forwards missing IDs: %+v", af)
	}
	// Prepare must NOT mutate config or start forwarders.
	if len(cfg.Domains) != 0 || len(cfg.Forwards) != 0 {
		t.Fatalf("config should be unchanged by Prepare: %+v", cfg)
	}
	if len(o.pool.Statuses()) != 0 {
		t.Fatal("no forwarder should be started by Prepare")
	}
}

func TestPrepareApplyAll_InvalidDomain(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)

	bad := config.Domain{ID: "x", Domain: "  "} // trim-empty
	_, _, _, _, err := o.PrepareApplyAll([]config.Domain{bad}, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "domain must not be empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareApplyAll_InvalidForward(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)

	bad := config.Forward{ID: "x", ListenPort: 0, TargetHost: "127.0.0.1", TargetPort: 8080}
	_, _, _, _, err := o.PrepareApplyAll(nil, []config.Forward{bad})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "listen_port") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareApplyAll_DuplicateListenPorts(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)

	port := forwarder.SuggestFreePort(testPortBase(t))
	if port == 0 {
		t.Fatal("no free port")
	}
	f1 := config.Forward{ID: "a", ListenPort: port, TargetHost: "127.0.0.1", TargetPort: 8080, Enabled: true}
	f2 := config.Forward{ID: "b", ListenPort: port, TargetHost: "127.0.0.1", TargetPort: 9090, Enabled: true}

	_, _, _, _, err := o.PrepareApplyAll(nil, []config.Forward{f1, f2})
	if err == nil {
		t.Fatal("expected duplicate-port error")
	}
	if !strings.Contains(err.Error(), "duplicate listen_port") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareApplyAll_ChangedFalseWhenZoneIdentical(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)

	d := config.Domain{ID: "d1", Domain: "keep.example.com"}
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: d.Domain}})
	// Re-seed the orchestrator's readHosts by constructing with the zone.
	cfg2 := config.NewDefaultConfig()
	var written string
	o2 := newOrch(t, cfg2, zone, &written)
	_ = o
	_, changed, _, _, err := o2.PrepareApplyAll([]config.Domain{d}, nil)
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when zone content identical")
	}
}

func TestPrepareApplyAll_WhilePaused(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.Settings.Paused = true
	// Pre-existing zone from a previous active state.
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "old.example.com"}})
	var written string
	o := newOrch(t, cfg, zone, &written)

	d := config.Domain{ID: "d1", Domain: "new.example.com"}
	f := freeForward(t, "127.0.0.1")
	content, changed, ad, af, err := o.PrepareApplyAll([]config.Domain{d}, []config.Forward{f})
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	// While paused the generated zone is empty; current zone is non-empty →
	// changed=true (the GUI will write an empty zone, clearing the stale
	// entries).
	if !changed {
		t.Fatal("expected changed=true; stale zone should be cleared")
	}
	if strings.Contains(content, "127.0.0.1") && strings.Contains(content, "new.example.com") {
		t.Fatalf("paused zone should NOT contain the new domain:\n%s", content)
	}
	// Mark "written" so the GUI's write clears the zone, then Complete.
	written = content
	if err := o.CompleteApplyAll(ad, af); err != nil {
		t.Fatalf("CompleteApplyAll: %v", err)
	}
	// Config is persisted.
	if len(cfg.Domains) != 1 || len(cfg.Forwards) != 1 {
		t.Fatalf("config not persisted: %+v", cfg)
	}
	// No forwards should be running while paused.
	if len(o.pool.Statuses()) != 0 {
		t.Fatalf("no forwards should run while paused, got %+v", o.pool.Statuses())
	}
}

// --- CompleteApplyAll ---

func TestCompleteApplyAll_Reconciles(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	// Initial: three enabled forwards (f1,f2,f3), all running.
	f1 := freeForwardAt(t, 0, "127.0.0.1")
	f2 := freeForwardAt(t, 100, "127.0.0.1")
	f3 := freeForwardAt(t, 200, "127.0.0.1")
	cfg.Forwards = []config.Forward{f1, f2, f3}
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "a.example.com"}}

	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "a.example.com"}})
	var written string
	o := newOrch(t, cfg, zone, &written)
	for _, f := range cfg.Forwards {
		if err := o.pool.StartWithRefresh(o.ctx, f.ID, f.ListenPort, f.TargetHost, f.TargetPort, time.Minute, nil); err != nil {
			t.Fatalf("start fwd %s: %v", f.ID, err)
		}
		waitForListening(t, o, f.ID)
	}

	// New desired set:
	//   f1: kept enabled, target edited in place
	//   f2: toggled disabled
	//   f3: REMOVED entirely
	//   f4: NEW, enabled
	f4 := freeForwardAt(t, 300, "127.0.0.1")
	newF1 := f1
	newF1.TargetPort = 9090
	newF2 := f2
	newF2.Enabled = false
	newDomains := []config.Domain{{ID: "d1", Domain: "a.example.com"}}
	newForwards := []config.Forward{newF1, newF2, f4}

	if err := o.CompleteApplyAll(newDomains, newForwards); err != nil {
		t.Fatalf("CompleteApplyAll: %v", err)
	}

	if len(cfg.Forwards) != 3 {
		t.Fatalf("config forwards = %d, want 3", len(cfg.Forwards))
	}
	for i, want := range newForwards {
		if cfg.Forwards[i].ID != want.ID {
			t.Fatalf("config[%d].ID = %q, want %q", i, cfg.Forwards[i].ID, want.ID)
		}
	}
	if cfg.Forwards[0].TargetPort != 9090 {
		t.Fatalf("in-place edit not applied: %d", cfg.Forwards[0].TargetPort)
	}

	waitForListening(t, o, f1.ID)
	waitForListening(t, o, f4.ID)
	if st, ok := o.pool.Statuses()[f2.ID]; ok {
		t.Fatalf("disabled forward should be stopped, status=%+v", st)
	}
	if st, ok := o.pool.Statuses()[f3.ID]; ok {
		t.Fatalf("removed forward should be stopped, status=%+v", st)
	}
}

// --- Pause / Resume ---

func TestPauseResume_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	d := config.Domain{ID: "d1", Domain: "svc.example.com"}
	f := freeForward(t, "127.0.0.1")
	cfg.Domains = []config.Domain{d}
	cfg.Forwards = []config.Forward{f}

	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: d.Domain}})
	var written string
	o := newOrch(t, cfg, zone, &written)
	if err := o.pool.StartWithRefresh(o.ctx, f.ID, f.ListenPort, f.TargetHost, f.TargetPort, time.Minute, nil); err != nil {
		t.Fatalf("start fwd: %v", err)
	}
	waitForListening(t, o, f.ID)

	// Pause: zone should become empty, changed=true.
	content, changed, err := o.PreparePause()
	if err != nil {
		t.Fatalf("PreparePause: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when pausing a non-empty zone")
	}
	if strings.Contains(content, d.Domain) {
		t.Fatalf("paused content should not contain domain:\n%s", content)
	}
	// Simulate the GUI's elevated write between Prepare and Complete.
	if err := o.writeHosts(content); err != nil {
		t.Fatalf("writeHosts: %v", err)
	}
	if err := o.CompletePause(); err != nil {
		t.Fatalf("CompletePause: %v", err)
	}
	if !cfg.Settings.Paused {
		t.Fatal("Paused should be true")
	}
	if len(o.pool.Statuses()) != 0 {
		t.Fatalf("all forwards should be stopped on pause, got %+v", o.pool.Statuses())
	}

	// Resume: zone should be rebuilt, changed=true.
	content2, changed2, err := o.PrepareResume()
	if err != nil {
		t.Fatalf("PrepareResume: %v", err)
	}
	if !changed2 {
		t.Fatal("expected changed=true when resuming into an empty zone")
	}
	if !strings.Contains(content2, "127.0.0.1 "+d.Domain) {
		t.Fatalf("resumed content should contain domain:\n%s", content2)
	}
	if err := o.writeHosts(content2); err != nil {
		t.Fatalf("writeHosts: %v", err)
	}
	if err := o.CompleteResume(); err != nil {
		t.Fatalf("CompleteResume: %v", err)
	}
	if cfg.Settings.Paused {
		t.Fatal("Paused should be false after resume")
	}
	// Enabled forward should be started again. Flags preserved.
	if cfg.Forwards[0].Enabled != true {
		t.Fatal("Enabled flag should be preserved across pause/resume")
	}
	waitForListening(t, o, f.ID)
}

func TestPreparePause_AlreadyEmpty(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)
	_, changed, err := o.PreparePause()
	if err != nil {
		t.Fatalf("PreparePause: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when zone already empty")
	}
}

// --- FixNow ---

func TestPrepareFixNow(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "fix1.example.com"}, {ID: "d2", Domain: "fix2.example.com"}}

	// Inconsistent: only d1 in hosts.
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "fix1.example.com"}})
	var written string
	o := newOrch(t, cfg, zone, &written)

	content, changed, err := o.PrepareFixNow()
	if err != nil {
		t.Fatalf("PrepareFixNow: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(content, "fix1.example.com") || !strings.Contains(content, "fix2.example.com") {
		t.Fatalf("written should contain both domains:\n%s", content)
	}
}

func TestCompleteFixNow(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	f1 := freeForwardAt(t, 0, "127.0.0.1")
	f2 := freeForwardAt(t, 100, "127.0.0.1")
	cfg.Forwards = []config.Forward{f1, f2}
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "fix.example.com"}}

	o := newOrch(t, cfg, "", nil)
	if err := o.CompleteFixNow(); err != nil {
		t.Fatalf("CompleteFixNow: %v", err)
	}
	waitForListening(t, o, f1.ID)
	waitForListening(t, o, f2.ID)
}

func TestFixNow_WhilePaused(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.Settings.Paused = true
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "paused.example.com"}}
	cfg.Forwards = []config.Forward{freeForward(t, "127.0.0.1")}

	// Stale non-empty zone while paused → inconsistent; FixNow must clear it.
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "paused.example.com"}})
	var written string
	o := newOrch(t, cfg, zone, &written)

	content, changed, err := o.PrepareFixNow()
	if err != nil {
		t.Fatalf("PrepareFixNow: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true; paused zone should be cleared")
	}
	if strings.Contains(content, "127.0.0.1") && strings.Contains(content, "paused.example.com") {
		t.Fatalf("paused FixNow content should be empty zone:\n%s", content)
	}
	// Simulate GUI write then CompleteFixNow.
	written = content
	if err := o.CompleteFixNow(); err != nil {
		t.Fatalf("CompleteFixNow: %v", err)
	}
	// No forwards should start while paused.
	if len(o.pool.Statuses()) != 0 {
		t.Fatalf("no forwards should run while paused, got %+v", o.pool.Statuses())
	}
}

// --- StartupCheck ---

func TestStartupCheck_PausedConsistent(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.Settings.Paused = true
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "p.example.com"}}
	cfg.Forwards = []config.Forward{freeForward(t, "127.0.0.1")}

	// Paused + empty zone → consistent. No forwards start.
	o := newOrch(t, cfg, "", nil)
	diff, err := o.StartupCheck()
	if err != nil {
		t.Fatalf("StartupCheck: %v", err)
	}
	if !diff.Consistent {
		t.Fatalf("expected consistent (paused+empty), got %+v", diff)
	}
	if len(o.pool.Statuses()) != 0 {
		t.Fatalf("no forwards should start while paused, got %+v", o.pool.Statuses())
	}
}

func TestStartupCheck_PausedInconsistent(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.Settings.Paused = true
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "p.example.com"}}
	cfg.Forwards = []config.Forward{freeForward(t, "127.0.0.1")}

	// Paused + non-empty zone → inconsistent (fix = empty zone).
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "p.example.com"}})
	o := newOrch(t, cfg, zone, nil)
	diff, err := o.StartupCheck()
	if err != nil {
		t.Fatalf("StartupCheck: %v", err)
	}
	if diff.Consistent {
		t.Fatal("expected inconsistent (paused+non-empty zone)")
	}
	// No forwards start when inconsistent.
	if len(o.pool.Statuses()) != 0 {
		t.Fatalf("no forwards should start when inconsistent, got %+v", o.pool.Statuses())
	}
}

func TestStartupCheck_ActiveConsistent(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	f1 := freeForwardAt(t, 0, "127.0.0.1")
	f2 := freeForwardAt(t, 100, "127.0.0.1")
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "one.example.com"}, {ID: "d2", Domain: "two.example.com"}}
	cfg.Forwards = []config.Forward{f1, f2}

	zone := hosts.BuildZone([]hosts.Entry{
		{IP: "127.0.0.1", Host: "one.example.com"},
		{IP: "127.0.0.1", Host: "two.example.com"},
	})
	o := newOrch(t, cfg, zone, nil)
	diff, err := o.StartupCheck()
	if err != nil {
		t.Fatalf("StartupCheck: %v", err)
	}
	if !diff.Consistent {
		t.Fatalf("expected consistent, got %+v", diff)
	}
	waitForListening(t, o, f1.ID)
	waitForListening(t, o, f2.ID)
}

func TestStartupCheck_ActiveInconsistent(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	f1 := freeForwardAt(t, 0, "127.0.0.1")
	f2 := freeForwardAt(t, 100, "127.0.0.1")
	cfg.Domains = []config.Domain{{ID: "d1", Domain: "one.example.com"}, {ID: "d2", Domain: "two.example.com"}}
	cfg.Forwards = []config.Forward{f1, f2}

	// Only one of two domains present in zone.
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: "one.example.com"}})
	o := newOrch(t, cfg, zone, nil)
	diff, err := o.StartupCheck()
	if err != nil {
		t.Fatalf("StartupCheck: %v", err)
	}
	if diff.Consistent {
		t.Fatal("expected inconsistent")
	}
	found := false
	for _, d := range diff.MissingInHosts {
		if d == "two.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected two.example.com in MissingInHosts: %+v", diff)
	}
	// No forwards start when inconsistent.
	if st, ok := o.pool.Statuses()[f1.ID]; ok && st.State == forwarder.StatusListening {
		t.Fatal("f1 forwarder should NOT be started when inconsistent")
	}
	if st, ok := o.pool.Statuses()[f2.ID]; ok && st.State == forwarder.StatusListening {
		t.Fatal("f2 forwarder should NOT be started when inconsistent")
	}
}

// --- SetSettings ---

func TestSetSettings_Clamp(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)

	if err := o.SetSettings(config.Settings{Paused: false, DNSRefreshMinutes: 0}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got := o.GetSettings().DNSRefreshMinutes; got != config.DefaultDNSRefreshMinutes {
		t.Fatalf("DNSRefreshMinutes = %d, want %d", got, config.DefaultDNSRefreshMinutes)
	}

	if err := o.SetSettings(config.Settings{Paused: true, DNSRefreshMinutes: -1}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got := o.GetSettings(); !got.Paused || got.DNSRefreshMinutes != config.DefaultDNSRefreshMinutes {
		t.Fatalf("settings wrong: %+v", got)
	}

	if err := o.SetSettings(config.Settings{Paused: false, DNSRefreshMinutes: 7}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got := o.GetSettings().DNSRefreshMinutes; got != 7 {
		t.Fatalf("DNSRefreshMinutes = %d, want 7", got)
	}
}

// --- ListDomains / ListForwards / CurrentHostsZone ---

func TestListDomainsForwards(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	d1 := config.Domain{ID: "d1", Domain: "a.example.com"}
	d2 := config.Domain{ID: "d2", Domain: "b.example.com"}
	f1 := freeForwardAt(t, 0, "127.0.0.1")
	cfg.Domains = []config.Domain{d1, d2}
	cfg.Forwards = []config.Forward{f1}

	o := newOrch(t, cfg, "", nil)

	gotD := o.ListDomains()
	if len(gotD) != 2 || gotD[0].ID != d1.ID || gotD[1].ID != d2.ID {
		t.Fatalf("ListDomains = %+v", gotD)
	}
	gotD[0].Domain = "mutated"
	if o.ListDomains()[0].Domain != d1.Domain {
		t.Fatal("ListDomains did not return a copy")
	}

	gotF := o.ListForwards()
	if len(gotF) != 1 || gotF[0].ID != f1.ID {
		t.Fatalf("ListForwards = %+v", gotF)
	}
	gotF[0].TargetHost = "mutated"
	if o.ListForwards()[0].TargetHost != f1.TargetHost {
		t.Fatal("ListForwards did not return a copy")
	}
}

func TestCurrentHostsZone(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	d := config.Domain{ID: "d1", Domain: "zone.example.com"}
	zone := hosts.BuildZone([]hosts.Entry{{IP: "127.0.0.1", Host: d.Domain}})
	o := newOrch(t, cfg, zone, nil)

	got, err := o.CurrentHostsZone()
	if err != nil {
		t.Fatalf("CurrentHostsZone: %v", err)
	}
	if !strings.Contains(got, hosts.BeginMarker) || !strings.Contains(got, d.Domain) {
		t.Fatalf("CurrentHostsZone = %q", got)
	}
}

func TestCurrentHostsZone_Empty(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	o := newOrch(t, cfg, "", nil)
	got, err := o.CurrentHostsZone()
	if err != nil {
		t.Fatalf("CurrentHostsZone: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty zone, got %q", got)
	}
}

func TestStatuses(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	f := freeForward(t, "127.0.0.1")
	cfg.Forwards = []config.Forward{f}
	o := newOrch(t, cfg, "", nil)

	if err := o.pool.StartWithRefresh(o.ctx, f.ID, f.ListenPort, f.TargetHost, f.TargetPort, time.Minute, nil); err != nil {
		t.Fatalf("start fwd: %v", err)
	}
	waitForListening(t, o, f.ID)

	st := o.Statuses()
	info, ok := st[f.ID]
	if !ok {
		t.Fatalf("Statuses missing %s: %+v", f.ID, st)
	}
	if info.State != forwarder.StatusListening {
		t.Fatalf("state = %v, want listening", info.State)
	}
}

// Ensure errors is referenced (used in other test files via shared helpers).
var _ = errors.Is