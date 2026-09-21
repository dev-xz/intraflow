//go:build darwin

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ---------------------- injectable hooks (for tests) ----------------------
//
// The darwin autostart path selection depends on (a) the running executable
// path, (b) whether SMAppService is available (macOS >= 13.0), and (c) the
// launchctl command runner. These are exposed as package-level vars so tests
// can swap them without touching the real system.

// darwinExePath returns the path of the running executable. Default
// implementation calls os.Executable; tests override to simulate bundled vs
// dev-binary paths.
var darwinExePath = func() (string, error) {
	return os.Executable()
}

// darwinSMAppServiceAvailable reports whether the SMAppService API is
// available at runtime (macOS >= 13.0). Default implementation calls into
// the cgo bridge in loginitem_darwin.go; tests override to simulate <13.
var darwinSMAppServiceAvailable = smAppServiceAvailable

// darwinRunLaunchctl runs a launchctl command and returns its combined
// output. Default implementation runs the real binary; tests override to
// fake responses.
var darwinRunLaunchctl = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

// ---------------------- errors ----------------------

// errTranslocated is returned when the app is running from a Gatekeeper App
// Translocation mount (a random, read-only path that exists only while the app
// runs). Registering SMAppService from there would store that ephemeral path as
// the login item, producing a registration that breaks after reboot.
var errTranslocated = errors.New("开机自启失败：应用正从 Gatekeeper 的临时随机路径（App Translocation）运行，重启后会失效。请先把 IntraFlow 拖入「应用程序」文件夹，执行 xattr -d com.apple.quarantine /Applications/intraflow.app 后重新打开，再开启开机自启")

// ---------------------- path selection ----------------------

// isBundled reports whether exePath is inside an .app bundle's MacOS dir, i.e.
// matches "*/<name>.app/Contents/MacOS/<binary>".
func isBundled(exePath string) bool {
	if exePath == "" {
		return false
	}
	// Normalize to forward slashes (macOS paths are already /, but be safe).
	exePath = filepath.ToSlash(exePath)
	parts := strings.Split(exePath, "/")
	// Need at least: ... <name>.app Contents MacOS <binary>
	if len(parts) < 4 {
		return false
	}
	if parts[len(parts)-2] != "MacOS" {
		return false
	}
	if parts[len(parts)-3] != "Contents" {
		return false
	}
	if !strings.HasSuffix(parts[len(parts)-4], ".app") {
		return false
	}
	return true
}

// isTranslocated reports whether exePath is inside a Gatekeeper App
// Translocation mount (…/AppTranslocation/<uuid>/d/App.app/…). Such a path is
// random and vanishes when the app exits, so it must never be used to register
// a login item.
func isTranslocated(exePath string) bool {
	return strings.Contains(filepath.ToSlash(exePath), "/AppTranslocation/")
}

// darwinUseSMAppService reports whether the SMAppService code path should be
// used: only when the executable is inside an .app bundle AND the
// SMAppService API is available (macOS >= 13).
func darwinUseSMAppService() bool {
	exe, err := darwinExePath()
	if err != nil {
		return false
	}
	if isTranslocated(exe) {
		return false
	}
	if !isBundled(exe) {
		return false
	}
	return darwinSMAppServiceAvailable()
}

// ---------------------- contract: GetAutoStartState ----------------------

func getAutoStartStateDarwin() (AutoStartState, error) {
	if darwinUseSMAppService() {
		status, err := smAppServiceStatus()
		if err != nil {
			// If SMAppService fails because we're somehow not bundled even
			// though the path looked bundled (ErrNotBundled), fall back to
			// the LaunchAgent check rather than hard-erroring.
			if errors.Is(err, errSMAppNotBundled) {
				return launchAgentState()
			}
			// Other SMAppService errors: fall back to LaunchAgent ground
			// truth (plist existence) but surface the underlying error so
			// callers can diagnose. State is best-effort.
			state, _ := launchAgentState()
			return state, fmt.Errorf("autostart: SMAppService status: %w", err)
		}
		return status, nil
	}
	return launchAgentState()
}

// launchAgentState reports the LaunchAgent ground-truth state (plist
// existence). Returns (disabled, nil) when no plist is present.
func launchAgentState() (AutoStartState, error) {
	p, err := launchAgentPath()
	if err != nil {
		return AutoStartStateDisabled, err
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AutoStartStateDisabled, nil
		}
		return AutoStartStateDisabled, err
	}
	return AutoStartStateEnabled, nil
}

// ---------------------- contract: SetAutoStart ----------------------

func setAutoStartDarwin(enabled bool) error {
	// Never register a login item from an App Translocation mount: the path is
	// random and disappears when the app exits, so the registration would be
	// broken after reboot. Only guard the enable path — disabling must keep
	// working so a user can remove a previously-created bad registration.
	if enabled {
		if exe, err := darwinExePath(); err == nil && isTranslocated(exe) {
			return errTranslocated
		}
	}
	if darwinUseSMAppService() {
		if err := smAppServiceSetEnabled(enabled); err != nil {
			// On ErrNotBundled, fall back to the LaunchAgent path.
			if errors.Is(err, errSMAppNotBundled) {
				return setAutoStartLaunchAgent(enabled)
			}
			return err
		}
		return nil
	}
	return setAutoStartLaunchAgent(enabled)
}

// ---------------------- contract: CleanupLegacyLaunchAgent ----------------------

func cleanupLegacyLaunchAgentDarwin() error {
	p, err := launchAgentPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("autostart: stat legacy plist: %w", err)
	}
	// Best-effort bootout before removing the file; surface errors but
	// tolerate "service not loaded"-style messages.
	uid := os.Getuid()
	out, err := darwinRunLaunchctl("bootout", fmt.Sprintf("gui/%d", uid), p)
	if err != nil {
		if !launchctlTolerantMessage(out, "not loaded", "no service", "not found", "Boot-out failed") {
			return fmt.Errorf("autostart: bootout legacy launch agent: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("autostart: remove legacy plist: %w", err)
	}
	return nil
}

// ---------------------- LaunchAgent fallback ----------------------

func setAutoStartLaunchAgent(enabled bool) error {
	if !enabled {
		return disableLaunchAgent()
	}
	return enableLaunchAgent()
}

func enableLaunchAgent() error {
	exe, err := currentExe()
	if err != nil {
		return err
	}
	dir, err := launchAgentDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("autostart: create LaunchAgents dir: %w", err)
	}
	plist := buildPlist(exe)
	p, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("autostart: write plist: %w", err)
	}
	uid := os.Getuid()
	out, err := darwinRunLaunchctl("bootstrap", fmt.Sprintf("gui/%d", uid), p)
	if err != nil {
		// Tolerate "already bootstrapped"-style messages: the agent is
		// already loaded this session, which is fine — the plist is in
		// place and will load at next login regardless.
		if launchctlTolerantMessage(out, "already bootstrapped", "already loaded") {
			return nil
		}
		return fmt.Errorf("autostart: bootstrap launch agent: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func disableLaunchAgent() error {
	p, err := launchAgentPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("autostart: stat plist: %w", err)
	}
	uid := os.Getuid()
	out, err := darwinRunLaunchctl("bootout", fmt.Sprintf("gui/%d", uid), p)
	if err != nil {
		// Tolerate "service not loaded"-style messages: the agent isn't
		// running this session, but we still want to remove the plist.
		if !launchctlTolerantMessage(out, "not loaded", "no service", "not found", "Boot-out failed") {
			return fmt.Errorf("autostart: bootout launch agent: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("autostart: remove plist: %w", err)
	}
	return nil
}

// launchctlTolerantMessage reports whether the launchctl output (combined
// stdout+stderr) contains any of the given tolerable substrings, indicating a
// non-fatal condition the caller should accept as success.
func launchctlTolerantMessage(out []byte, tolerable ...string) bool {
	s := strings.ToLower(string(out))
	for _, t := range tolerable {
		if strings.Contains(s, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// ---------------------- shared helpers ----------------------

// launchAgentDir returns ~/Library/LaunchAgents. Uses os.UserHomeDir so
// tests can override HOME via t.Setenv("HOME", ...).
func launchAgentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("autostart: resolve home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func launchAgentPath() (string, error) {
	dir, err := launchAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, autostartLabel+".plist"), nil
}

// buildPlist returns the LaunchAgent plist XML for the given executable
// path. Factored out so tests can assert on its content without touching
// ~/Library/LaunchAgents.
//
// KeepAlive is false: if the user quits IntraFlow we do not want launchd to
// respawn it. RunAtLoad is true so the agent starts the moment it is loaded
// (and at every login).
func buildPlist(exePath string) string {
	escaped := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	).Replace(exePath)
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + autostartLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + escaped + `</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <false/>
</dict>
</plist>
`
}