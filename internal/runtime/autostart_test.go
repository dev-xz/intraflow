package runtime

import (
	"strings"
	"testing"
)

// TestBuildDesktopFile asserts the XDG autostart desktop file contains the
// executable path and the required keys. (Cross-platform: the linux helper is
// built on every platform in this package's non-darwin files... but actually
// buildDesktopFile lives in autostart.go which is cross-platform, so this
// test compiles everywhere.)
func TestBuildDesktopFile(t *testing.T) {
	exe := "/usr/bin/intraflow"
	content := buildDesktopFile(exe)

	if !strings.Contains(content, "Exec="+exe) {
		t.Errorf("desktop file missing Exec=exe path")
	}
	if !strings.Contains(content, "Terminal=false") {
		t.Errorf("desktop file missing Terminal=false")
	}
	if !strings.Contains(content, "X-GNOME-Autostart-enabled=true") {
		t.Errorf("desktop file missing X-GNOME-Autostart-enabled=true")
	}
	if !strings.Contains(content, "Name=IntraFlow") {
		t.Errorf("desktop file missing Name=IntraFlow")
	}
}

// TestAutoStartStateValues asserts the exported string constants match the
// contract so the parallel lane's string comparisons are stable.
func TestAutoStartStateValues(t *testing.T) {
	if AutoStartStateEnabled != "enabled" {
		t.Errorf("AutoStartStateEnabled = %q, want %q", AutoStartStateEnabled, "enabled")
	}
	if AutoStartStateDisabled != "disabled" {
		t.Errorf("AutoStartStateDisabled = %q, want %q", AutoStartStateDisabled, "disabled")
	}
	if AutoStartStateRequiresApproval != "requires_approval" {
		t.Errorf("AutoStartStateRequiresApproval = %q, want %q", AutoStartStateRequiresApproval, "requires_approval")
	}
}