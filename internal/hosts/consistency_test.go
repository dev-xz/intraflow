package hosts

import (
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
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

// decodeEncodedCommandArg extracts the value following "-EncodedCommand" in the
// outer PowerShell script, base64-decodes it, and decodes the bytes as
// UTF-16LE, returning the inner script text. It fails the test if the argument
// is missing or malformed. Kept in the test file because the production code
// only ever encodes; nothing decodes.
func decodeEncodedCommandArg(t *testing.T, outer string) string {
	t.Helper()

	// In the ArgumentList array the pair appears as
	// '-EncodedCommand', '<base64>'. Match the whole delimiter so we land
	// directly on the opening quote of the base64 value.
	const marker = "'-EncodedCommand', '"
	idx := strings.Index(outer, marker)
	if idx < 0 {
		t.Fatalf("outer script missing %q: %q", marker, outer)
	}
	rest := outer[idx+len(marker):]

	close := strings.Index(rest, "'")
	if close < 0 {
		t.Fatalf("unterminated quoted value after %q in %q", marker, outer)
	}
	encoded := rest[:close]

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", encoded, err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16LE payload has odd length %d", len(raw))
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// TestBuildWindowsElevateCommand verifies the pure Windows command builder:
// the executable is powershell, the outer script raises UAC via Start-Process
// -Verb RunAs and maps cancellation to 1223, and the inner copy script (passed
// via -EncodedCommand) copies the source over the target. The builder takes no
// runtime.GOOS dependency, so this runs identically on macOS/Linux CI.
func TestBuildWindowsElevateCommand(t *testing.T) {
	target := `C:\Windows\System32\drivers\etc\hosts`
	source := `C:\Users\me\AppData\Local\Temp\intraflow-hosts-123.txt`

	name, args := buildWindowsElevateCommand(target, source)
	if name != "powershell" {
		t.Fatalf("name = %q, want \"powershell\"", name)
	}
	if len(args) < 3 || args[0] != "-NoProfile" || args[1] != "-NonInteractive" || args[2] != "-Command" {
		t.Fatalf("expected args to start with -NoProfile -NonInteractive -Command, got %+v", args)
	}

	outer := strings.Join(args, " ")
	for _, want := range []string{"Start-Process", "-Verb RunAs", "1223"} {
		if !strings.Contains(outer, want) {
			t.Fatalf("outer script missing %q: %q", want, outer)
		}
	}

	inner := decodeEncodedCommandArg(t, outer)
	for _, want := range []string{"Copy-Item", "-Force", source, target} {
		if !strings.Contains(inner, want) {
			t.Fatalf("inner script missing %q: %q", want, inner)
		}
	}
}

// TestBuildWindowsElevateCommand_EscapesSingleQuote checks that a source path
// containing a single quote is doubled when embedded in the PowerShell
// single-quoted literal, so the inner script remains syntactically valid.
func TestBuildWindowsElevateCommand_EscapesSingleQuote(t *testing.T) {
	target := `C:\Windows\System32\drivers\etc\hosts`
	source := `C:\Users\o'brien\Temp\x.txt`

	_, args := buildWindowsElevateCommand(target, source)
	outer := strings.Join(args, " ")
	inner := decodeEncodedCommandArg(t, outer)

	want := `C:\Users\o''brien\Temp\x.txt`
	if !strings.Contains(inner, want) {
		t.Fatalf("inner script missing escaped path %q: %q", want, inner)
	}
	if strings.Contains(inner, `o'brien`) {
		t.Fatalf("inner script contains unescaped single quote: %q", inner)
	}
}

// TestIsCancellationExitCode documents the exit-code contract used to detect a
// declined UAC prompt: 1223 (Win32 ERROR_CANCELLED) is cancellation; success
// (0) and other failures (1, 5) are not.
func TestIsCancellationExitCode(t *testing.T) {
	if !isCancellationExitCode(1223) {
		t.Fatalf("isCancellationExitCode(1223) = false, want true")
	}
	for _, code := range []int{0, 1, 5} {
		if isCancellationExitCode(code) {
			t.Fatalf("isCancellationExitCode(%d) = true, want false", code)
		}
	}
}