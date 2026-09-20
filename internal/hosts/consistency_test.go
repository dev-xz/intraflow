package hosts

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestCheckConsistency_Consistent: config domains match the zone exactly.
func TestCheckConsistency_Consistent(t *testing.T) {
	config := []string{"a.example.com", "b.example.com"}
	content := BeginMarker + "\n" +
		"127.0.0.1 a.example.com\n" +
		"127.0.0.1 b.example.com\n" +
		EndMarker + "\n"

	diff := CheckConsistency(config, content)
	if !diff.Consistent {
		t.Fatalf("expected Consistent=true, got %+v", diff)
	}
	if len(diff.MissingInHosts) != 0 || len(diff.MissingInConfig) != 0 {
		t.Fatalf("expected empty diff slices, got %+v", diff)
	}
}

// TestCheckConsistency_ConfigHasMore: a domain in config absent from the zone
// shows up in MissingInHosts.
func TestCheckConsistency_ConfigHasMore(t *testing.T) {
	config := []string{"a.example.com", "b.example.com"}
	content := BeginMarker + "\n" +
		"127.0.0.1 a.example.com\n" +
		EndMarker + "\n"

	diff := CheckConsistency(config, content)
	if diff.Consistent {
		t.Fatalf("expected Consistent=false, got %+v", diff)
	}
	if len(diff.MissingInHosts) != 1 || diff.MissingInHosts[0] != "b.example.com" {
		t.Fatalf("MissingInHosts mismatch: %+v", diff.MissingInHosts)
	}
	if len(diff.MissingInConfig) != 0 {
		t.Fatalf("expected no MissingInConfig, got %+v", diff.MissingInConfig)
	}
}

// TestCheckConsistency_HostsHasResidual: a domain in the zone absent from
// config shows up in MissingInConfig.
func TestCheckConsistency_HostsHasResidual(t *testing.T) {
	config := []string{"a.example.com"}
	content := BeginMarker + "\n" +
		"127.0.0.1 a.example.com\n" +
		"127.0.0.1 stale.example.com\n" +
		EndMarker + "\n"

	diff := CheckConsistency(config, content)
	if diff.Consistent {
		t.Fatalf("expected Consistent=false, got %+v", diff)
	}
	if len(diff.MissingInConfig) != 1 || diff.MissingInConfig[0] != "stale.example.com" {
		t.Fatalf("MissingInConfig mismatch: %+v", diff.MissingInConfig)
	}
	if len(diff.MissingInHosts) != 0 {
		t.Fatalf("expected no MissingInHosts, got %+v", diff.MissingInHosts)
	}
}

// TestIsCancellation_Wrapped asserts IsCancellation returns true for a wrapped
// ErrElevationCancelled and false for a generic error.
func TestIsCancellation_Wrapped(t *testing.T) {
	wrapped := wrapErr("write failed: %w", ErrElevationCancelled)
	if !IsCancellation(wrapped) {
		t.Fatalf("expected IsCancellation=true for wrapped ErrElevationCancelled")
	}

	generic := wrapErr("some other failure: %w", errGeneric)
	if IsCancellation(generic) {
		t.Fatalf("expected IsCancellation=false for generic error")
	}

	if !IsCancellation(ErrElevationCancelled) {
		t.Fatalf("expected IsCancellation=true for bare ErrElevationCancelled")
	}
}

// errGeneric is a sentinel used only by TestIsCancellation_Wrapped.
var errGeneric = errors.New("generic failure")

// wrapErr helper kept for clarity; uses fmt.Errorf for %w wrapping.
func wrapErr(format string, err error) error { return fmt.Errorf(format, err) }

// TestBuildElevateCommand_Darwin asserts that on darwin the produced command
// invokes osascript with "with administrator privileges". On other platforms
// the test asserts the platform-appropriate executable name so it does not
// silently skip.
func TestBuildElevateCommand_Darwin(t *testing.T) {
	name, args := buildElevateCommand("/etc/hosts", "/tmp/intraflow-hosts-xxx.txt")

	switch runtime.GOOS {
	case "darwin":
		// On darwin we call osascript directly (NOT wrapped in sudo). sudo
		// requires a TTY for its password prompt, which a GUI app launched
		// from the dock does not have; osascript's own
		// "with administrator privileges" triggers the native GUI admin
		// dialog without needing a terminal.
		if name != "osascript" {
			t.Fatalf("name = %q, want \"osascript\"", name)
		}
		if len(args) < 2 || args[0] != "-e" {
			t.Fatalf("expected args[0]=-e, got %+v", args)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "with administrator privileges") {
			t.Fatalf("expected 'with administrator privileges' in args, got %q", joined)
		}
		if !strings.Contains(joined, "/etc/hosts") {
			t.Fatalf("expected target path in args, got %q", joined)
		}
	case "linux":
		if name != "pkexec" {
			t.Fatalf("name = %q, want \"pkexec\"", name)
		}
	case "windows":
		if name != "powershell" {
			t.Fatalf("name = %q, want \"powershell\" (MVP stub)", name)
		}
	default:
		if name != "sudo" {
			t.Fatalf("name = %q, want \"sudo\" (fallback)", name)
		}
	}
}