package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// AutoStart is a namespace for the OS-native auto-start-on-boot controls.
// All methods are package-level functions; the AutoStart type exists only so
// the API reads as a coherent group and matches the spec's naming.
type AutoStart struct{}

// autostartLabel is the LaunchAgent label / Run-registry value name / desktop
// file basename used across platforms.
const autostartLabel = "com.intraflow.app"

// AutoStartState is the system ground-truth login-item state.
type AutoStartState string

const (
	// AutoStartStateEnabled: the login item is registered and approved.
	AutoStartStateEnabled AutoStartState = "enabled"
	// AutoStartStateDisabled: no login item is registered.
	AutoStartStateDisabled AutoStartState = "disabled"
	// AutoStartStateRequiresApproval: the login item is registered but the
	// user has not yet approved it in System Settings > Login Items
	// (SMAppService "requires approval" state).
	AutoStartStateRequiresApproval AutoStartState = "requires_approval"
)

// ----------------------- exported API (contract) -----------------------

// GetAutoStartState queries the live system state (SMAppService on bundled
// macOS 13+, LaunchAgent fallback otherwise; platform APIs on Windows/Linux).
func GetAutoStartState() (AutoStartState, error) {
	switch runtime.GOOS {
	case "darwin":
		return getAutoStartStateDarwin()
	case "windows":
		return getAutoStartStateWindows()
	case "linux":
		return getAutoStartStateLinux()
	default:
		return AutoStartStateDisabled, errors.New("autostart not implemented on " + runtime.GOOS)
	}
}

// SetAutoStart registers or unregisters the login item via the same path
// selection as GetAutoStartState.
func SetAutoStart(enabled bool) error {
	switch runtime.GOOS {
	case "darwin":
		return setAutoStartDarwin(enabled)
	case "windows":
		return setAutoStartWindows(enabled)
	case "linux":
		return setAutoStartLinux(enabled)
	default:
		return errors.New("autostart not implemented on " + runtime.GOOS)
	}
}

// CleanupLegacyLaunchAgent removes a stale
// ~/Library/LaunchAgents/com.intraflow.app.plist (bootout + delete). No-op
// (nil error) when absent. Errors are reported, not swallowed. On non-darwin
// platforms this is a no-op returning nil.
func CleanupLegacyLaunchAgent() error {
	switch runtime.GOOS {
	case "darwin":
		return cleanupLegacyLaunchAgentDarwin()
	default:
		return nil
	}
}

// ----------------------- legacy wrappers -----------------------
//
// Keep the previously-exported IsAutoStartEnabled / EnableAutoStart /
// DisableAutoStart compiling (host.go still calls them until the host lane
// rewires them onto GetAutoStartState / SetAutoStart). They are thin wrappers
// over the new contract.

// IsAutoStartEnabled reports whether IntraFlow is currently registered to
// launch at login/boot. Returns true only when GetAutoStartState reports
// AutoStartStateEnabled.
func IsAutoStartEnabled() (bool, error) {
	state, err := GetAutoStartState()
	if err != nil {
		return false, err
	}
	return state == AutoStartStateEnabled, nil
}

// EnableAutoStart registers IntraFlow to launch at login/boot.
func EnableAutoStart() error { return SetAutoStart(true) }

// DisableAutoStart unregisters IntraFlow from launching at login/boot.
func DisableAutoStart() error { return SetAutoStart(false) }

// currentExe returns the path to the running executable. On macOS this is the
// binary inside the .app bundle; the LaunchAgent will relaunch that binary.
// (For a .app launch the user would typically point at the bundle, but the
// binary path works for LaunchAgents and avoids needing to resolve the
// bundle root.)
func currentExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("autostart: resolve executable: %w", err)
	}
	return exe, nil
}

// ----------------------------- windows -----------------------------

// windowsRunValueName is the registry value name under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run used for auto-start.
const windowsRunValueName = "IntraFlow"

// windowsRunKey is the full registry key path (HKCU\...).
const windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func getAutoStartStateWindows() (AutoStartState, error) {
	cmd := exec.Command("reg.exe", "QUERY", windowsRunKey, "/v", windowsRunValueName)
	if err := cmd.Run(); err != nil {
		// reg.exe exits non-zero when the value is absent.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return AutoStartStateDisabled, nil
		}
		return AutoStartStateDisabled, fmt.Errorf("autostart: reg query: %w", err)
	}
	return AutoStartStateEnabled, nil
}

func setAutoStartWindows(enabled bool) error {
	if !enabled {
		cmd := exec.Command("reg.exe", "DELETE", windowsRunKey,
			"/v", windowsRunValueName, "/f")
		if out, err := cmd.CombinedOutput(); err != nil {
			// If the value doesn't exist, DELETE exits non-zero; treat that as
			// success (idempotent).
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return nil
			}
			return fmt.Errorf("autostart: reg delete: %w: %s", err, string(out))
		}
		return nil
	}
	exe, err := currentExe()
	if err != nil {
		return err
	}
	cmd := exec.Command("reg.exe", "ADD", windowsRunKey,
		"/v", windowsRunValueName, "/t", "REG_SZ", "/d", exe, "/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("autostart: reg add: %w: %s", err, string(out))
	}
	return nil
}

// ----------------------------- linux -----------------------------

// linuxAutostartDir returns ~/.config/autostart. Uses os.UserHomeDir so
// tests can override HOME.
func linuxAutostartDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("autostart: resolve home: %w", err)
	}
	return filepath.Join(home, ".config", "autostart"), nil
}

func linuxDesktopPath() (string, error) {
	dir, err := linuxAutostartDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "intraflow.desktop"), nil
}

func getAutoStartStateLinux() (AutoStartState, error) {
	p, err := linuxDesktopPath()
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

func setAutoStartLinux(enabled bool) error {
	if !enabled {
		p, err := linuxDesktopPath()
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("autostart: remove desktop file: %w", err)
		}
		return nil
	}
	exe, err := currentExe()
	if err != nil {
		return err
	}
	dir, err := linuxAutostartDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("autostart: create autostart dir: %w", err)
	}
	content := buildDesktopFile(exe)
	p, err := linuxDesktopPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return fmt.Errorf("autostart: write desktop file: %w", err)
	}
	return nil
}

// buildDesktopFile returns the XDG autostart .desktop file content for the
// given executable. Factored out for testability.
func buildDesktopFile(exePath string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=IntraFlow\n" +
		"Comment=Local hosts redirection and TCP port forwarding\n" +
		"Exec=" + exePath + "\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
}