//go:build darwin

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// autostartTestHooks holds saved hook values so tests can restore them.
type autostartTestHooks struct {
	exePath   func() (string, error)
	available func() bool
	runCtl    func(args ...string) ([]byte, error)
}

func saveAutostartHooks(t *testing.T) autostartTestHooks {
	t.Helper()
	saved := autostartTestHooks{
		exePath:   darwinExePath,
		available: darwinSMAppServiceAvailable,
		runCtl:    darwinRunLaunchctl,
	}
	// Always restore at end of test.
	t.Cleanup(func() {
		darwinExePath = saved.exePath
		darwinSMAppServiceAvailable = saved.available
		darwinRunLaunchctl = saved.runCtl
	})
	return saved
}

// withTempHome sets HOME to a temp dir for the duration of the test so the
// LaunchAgents path resolves under it instead of the real ~/Library.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

// ----------------------- isBundled -----------------------

func TestIsBundled_Bundled(t *testing.T) {
	if !isBundled("/Applications/IntraFlow.app/Contents/MacOS/intraflow") {
		t.Errorf("expected bundled path to be detected as bundled")
	}
}

func TestIsBundled_DevBinary(t *testing.T) {
	if isBundled("/Users/foo/go/bin/intraflow") {
		t.Errorf("expected dev-binary path to NOT be detected as bundled")
	}
	if isBundled("/Users/foo/Project/intraflow/intraflow") {
		t.Errorf("expected project-dir binary path to NOT be detected as bundled")
	}
}

func TestIsBundled_EmptyAndShort(t *testing.T) {
	if isBundled("") {
		t.Errorf("empty path should not be bundled")
	}
	if isBundled("intraflow") {
		t.Errorf("bare name should not be bundled")
	}
	if isBundled("/a/b/intraflow") {
		t.Errorf("short path should not be bundled")
	}
}

// ----------------------- buildPlist (darwin) -----------------------

func TestBuildPlistContents(t *testing.T) {
	exe := "/Applications/IntraFlow.app/Contents/MacOS/intraflow"
	plist := buildPlist(exe)

	if !strings.Contains(plist, "<key>Label</key>") {
		t.Errorf("plist missing Label key")
	}
	if !strings.Contains(plist, autostartLabel) {
		t.Errorf("plist missing label value %q", autostartLabel)
	}
	if !strings.Contains(plist, "<key>ProgramArguments</key>") {
		t.Errorf("plist missing ProgramArguments key")
	}
	if !strings.Contains(plist, exe) {
		t.Errorf("plist missing executable path %q", exe)
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Errorf("plist missing RunAtLoad key")
	}
	if !strings.Contains(plist, "<true/>") {
		t.Errorf("plist missing <true/> for RunAtLoad")
	}
	if !strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Errorf("plist missing KeepAlive key")
	}
	if !strings.Contains(plist, "<false/>") {
		t.Errorf("plist missing <false/> for KeepAlive")
	}
}

func TestBuildPlistEscapesXML(t *testing.T) {
	plist := buildPlist(`/path/with/&/and/<>/exe`)
	if !strings.Contains(plist, "&amp;") {
		t.Errorf("plist did not escape &")
	}
	if !strings.Contains(plist, "&lt;") {
		t.Errorf("plist did not escape <")
	}
	if !strings.Contains(plist, "&gt;") {
		t.Errorf("plist did not escape >")
	}
}

// ----------------------- path selection -----------------------

func TestDarwinUseSMAppService_BundledAnd13(t *testing.T) {
	hooks := saveAutostartHooks(t)
	_ = hooks
	darwinExePath = func() (string, error) {
		return "/Applications/IntraFlow.app/Contents/MacOS/intraflow", nil
	}
	darwinSMAppServiceAvailable = func() bool { return true }
	if !darwinUseSMAppService() {
		t.Errorf("expected SMAppService path when bundled + available")
	}
}

func TestDarwinUseSMAppService_DevBinary(t *testing.T) {
	saveAutostartHooks(t)
	darwinExePath = func() (string, error) {
		return "/Users/foo/go/bin/intraflow", nil
	}
	darwinSMAppServiceAvailable = func() bool { return true }
	if darwinUseSMAppService() {
		t.Errorf("expected LaunchAgent fallback when not bundled, even if macOS >= 13")
	}
}

func TestDarwinUseSMAppService_BundledButUnder13(t *testing.T) {
	saveAutostartHooks(t)
	darwinExePath = func() (string, error) {
		return "/Applications/IntraFlow.app/Contents/MacOS/intraflow", nil
	}
	darwinSMAppServiceAvailable = func() bool { return false }
	if darwinUseSMAppService() {
		t.Errorf("expected LaunchAgent fallback on macOS < 13 even if bundled")
	}
}

func TestDarwinUseSMAppService_ExeError(t *testing.T) {
	saveAutostartHooks(t)
	darwinExePath = func() (string, error) {
		return "", errors.New("boom")
	}
	darwinSMAppServiceAvailable = func() bool { return true }
	if darwinUseSMAppService() {
		t.Errorf("expected fallback when executable path lookup fails")
	}
}

// ----------------------- state mapping (GetAutoStartState via fallback) -----------------------

func TestGetAutoStartState_Fallback_NoPlist(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }
	darwinSMAppServiceAvailable = func() bool { return true }

	state, err := GetAutoStartState()
	if err != nil {
		t.Fatalf("GetAutoStartState: %v", err)
	}
	if state != AutoStartStateDisabled {
		t.Errorf("expected disabled when no plist exists, got %q", state)
	}
}

func TestGetAutoStartState_Fallback_PlistPresent(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }
	darwinSMAppServiceAvailable = func() bool { return true }

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, autostartLabel+".plist"), []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}

	state, err := GetAutoStartState()
	if err != nil {
		t.Fatalf("GetAutoStartState: %v", err)
	}
	if state != AutoStartStateEnabled {
		t.Errorf("expected enabled when plist exists, got %q", state)
	}
}

// ----------------------- LaunchAgent fallback: enable (bootstrap) -----------------------

func TestEnableLaunchAgent_BootstrapSuccess(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	var called []string
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		called = append(called, strings.Join(args, " "))
		return []byte(""), nil
	}

	if err := enableLaunchAgent(); err != nil {
		t.Fatalf("enableLaunchAgent: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("expected 1 launchctl call, got %d (%v)", len(called), called)
	}
	if !strings.HasPrefix(called[0], "bootstrap gui/") {
		t.Errorf("expected bootstrap command, got %q", called[0])
	}
	if !strings.HasSuffix(called[0], ".plist") {
		t.Errorf("expected plist path in bootstrap command, got %q", called[0])
	}

	// Plist should now exist.
	p, _ := launchAgentPath()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("plist not written: %v", err)
	}
}

func TestEnableLaunchAgent_BootstrapAlreadyLoaded(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("service already bootstrapped"), errors.New("exit 5")
	}

	if err := enableLaunchAgent(); err != nil {
		t.Errorf("expected nil error for already-bootstrapped, got %v", err)
	}
	// Plist still written.
	p, _ := launchAgentPath()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("plist not written on tolerant failure: %v", err)
	}
}

func TestEnableLaunchAgent_BootstrapHardFailure(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("some fatal error from launchctl"), errors.New("exit 1")
	}

	err := enableLaunchAgent()
	if err == nil {
		t.Fatalf("expected error on hard bootstrap failure, got nil")
	}
	if !strings.Contains(err.Error(), "bootstrap launch agent") {
		t.Errorf("error should mention bootstrap, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "some fatal error from launchctl") {
		t.Errorf("error should include launchctl output, got %q", err.Error())
	}
}

// ----------------------- LaunchAgent fallback: disable (bootout) -----------------------

func TestDisableLaunchAgent_NoPlist(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)

	called := false
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	if err := disableLaunchAgent(); err != nil {
		t.Fatalf("disableLaunchAgent: %v", err)
	}
	if called {
		t.Errorf("launchctl should not be called when plist absent")
	}
}

func TestDisableLaunchAgent_BootoutSuccess(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	// Pre-create the plist.
	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var called []string
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		called = append(called, strings.Join(args, " "))
		return []byte(""), nil
	}

	if err := disableLaunchAgent(); err != nil {
		t.Fatalf("disableLaunchAgent: %v", err)
	}
	if len(called) != 1 || !strings.HasPrefix(called[0], "bootout gui/") {
		t.Errorf("expected one bootout call, got %v", called)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist should be removed, stat err=%v", err)
	}
}

func TestDisableLaunchAgent_BootoutNotLoadedTolerated(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("Boot-out failed: 128: service not loaded"), errors.New("exit 128")
	}

	if err := disableLaunchAgent(); err != nil {
		t.Errorf("expected nil error for not-loaded, got %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist should still be removed on tolerant bootout failure, stat err=%v", err)
	}
}

func TestDisableLaunchAgent_BootoutHardFailure(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("some unrecoverable error"), errors.New("exit 9")
	}

	err := disableLaunchAgent()
	if err == nil {
		t.Fatalf("expected error on hard bootout failure, got nil")
	}
	if !strings.Contains(err.Error(), "bootout launch agent") {
		t.Errorf("error should mention bootout, got %q", err.Error())
	}
	// Plist should NOT be removed on hard failure.
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("plist should remain on hard bootout failure, got stat err=%v", statErr)
	}
}

// ----------------------- SetAutoStart path selection -----------------------

func TestSetAutoStart_FallbackEnable(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }
	darwinSMAppServiceAvailable = func() bool { return true }

	ctlCalled := false
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		ctlCalled = true
		if args[0] != "bootstrap" {
			t.Errorf("expected bootstrap, got %q", args[0])
		}
		return nil, nil
	}

	if err := SetAutoStart(true); err != nil {
		t.Fatalf("SetAutoStart(true): %v", err)
	}
	if !ctlCalled {
		t.Errorf("expected LaunchAgent fallback to run launchctl")
	}
}

func TestSetAutoStart_FallbackDisable(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }
	darwinSMAppServiceAvailable = func() bool { return true }

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		if args[0] != "bootout" {
			t.Errorf("expected bootout, got %q", args[0])
		}
		return nil, nil
	}

	if err := SetAutoStart(false); err != nil {
		t.Fatalf("SetAutoStart(false): %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist should be removed after disable, stat err=%v", err)
	}
}

// ----------------------- CleanupLegacyLaunchAgent -----------------------

func TestCleanupLegacyLaunchAgent_Absent(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)

	called := false
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	if err := CleanupLegacyLaunchAgent(); err != nil {
		t.Fatalf("CleanupLegacyLaunchAgent: %v", err)
	}
	if called {
		t.Errorf("launchctl should not run when plist absent")
	}
}

func TestCleanupLegacyLaunchAgent_Present(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var called []string
	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		called = append(called, strings.Join(args, " "))
		return nil, nil
	}

	if err := CleanupLegacyLaunchAgent(); err != nil {
		t.Fatalf("CleanupLegacyLaunchAgent: %v", err)
	}
	if len(called) != 1 || !strings.HasPrefix(called[0], "bootout gui/") {
		t.Errorf("expected one bootout call, got %v", called)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy plist should be removed, stat err=%v", err)
	}
}

func TestCleanupLegacyLaunchAgent_NotLoadedTolerated(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("service not loaded"), errors.New("exit 1")
	}

	if err := CleanupLegacyLaunchAgent(); err != nil {
		t.Errorf("expected nil error for not-loaded, got %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist should still be removed on tolerant failure, stat err=%v", err)
	}
}

func TestCleanupLegacyLaunchAgent_HardFailure(t *testing.T) {
	saveAutostartHooks(t)
	tmp := withTempHome(t)

	dir := filepath.Join(tmp, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p, _ := launchAgentPath()
	if err := os.WriteFile(p, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	darwinRunLaunchctl = func(args ...string) ([]byte, error) {
		return []byte("permission denied by SIP"), errors.New("exit 1")
	}

	err := CleanupLegacyLaunchAgent()
	if err == nil {
		t.Fatalf("expected error on hard bootout failure, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied by SIP") {
		t.Errorf("error should include launchctl output, got %q", err.Error())
	}
	// Plist should NOT be removed on hard failure.
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("plist should remain on hard bootout failure, got stat err=%v", statErr)
	}
}

// ----------------------- launchctlTolerantMessage -----------------------

func TestLaunchctlTolerantMessage(t *testing.T) {
	cases := []struct {
		out      string
		tolerant []string
		want     bool
	}{
		{"service already bootstrapped", []string{"already bootstrapped"}, true},
		{"Service Already Bootstrapped", []string{"already bootstrapped"}, true},
		{"nothing to do", []string{"already bootstrapped"}, false},
		{"Boot-out failed: service not loaded", []string{"not loaded", "not found"}, true},
		{"", []string{"not loaded"}, false},
	}
	for _, c := range cases {
		got := launchctlTolerantMessage([]byte(c.out), c.tolerant...)
		if got != c.want {
			t.Errorf("launchctlTolerantMessage(%q, %v) = %v, want %v", c.out, c.tolerant, got, c.want)
		}
	}
}

// ----------------------- legacy wrappers -----------------------

func TestLegacyWrappers(t *testing.T) {
	saveAutostartHooks(t)
	withTempHome(t)
	darwinExePath = func() (string, error) { return "/Users/foo/go/bin/intraflow", nil }
	darwinSMAppServiceAvailable = func() bool { return true }
	darwinRunLaunchctl = func(args ...string) ([]byte, error) { return nil, nil }

	enabled, err := IsAutoStartEnabled()
	if err != nil {
		t.Fatalf("IsAutoStartEnabled: %v", err)
	}
	if enabled {
		t.Errorf("expected false when no plist exists")
	}
	if err := EnableAutoStart(); err != nil {
		t.Fatalf("EnableAutoStart: %v", err)
	}
	enabled, err = IsAutoStartEnabled()
	if err != nil {
		t.Fatalf("IsAutoStartEnabled: %v", err)
	}
	if !enabled {
		t.Errorf("expected true after EnableAutoStart")
	}
	if err := DisableAutoStart(); err != nil {
		t.Fatalf("DisableAutoStart: %v", err)
	}
	enabled, err = IsAutoStartEnabled()
	if err != nil {
		t.Fatalf("IsAutoStartEnabled: %v", err)
	}
	if enabled {
		t.Errorf("expected false after DisableAutoStart")
	}
}