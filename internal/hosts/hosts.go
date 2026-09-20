// Package hosts manages the system hosts file (/etc/hosts on macOS/Linux,
// C:\Windows\System32\drivers\etc\hosts on Windows) for IntraFlow.
//
// All IntraFlow-managed entries live inside a marked region delimited by the
// BeginMarker and EndMarker constants. Updates replace the entire marked
// region, preserving any content outside it verbatim. Writing the hosts file
// requires administrator privileges, which are obtained transiently via the
// platform's native elevation mechanism (osascript on macOS, pkexec/sudo on
// Linux, runas/PowerShell on Windows).
package hosts

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// BeginMarker marks the start of the IntraFlow-managed region in the hosts
// file. Everything between BeginMarker and EndMarker is owned by IntraFlow and
// replaced wholesale on every update.
const BeginMarker = "# BEGIN IntraFlow (do not edit)"

// EndMarker marks the end of the IntraFlow-managed region in the hosts file.
const EndMarker = "# END IntraFlow"

// ErrElevationCancelled is returned by WriteHostsElevated when the user
// cancels the privilege-elevation prompt (e.g. clicks Cancel in the macOS
// osascript dialog or otherwise dismisses the prompt).
var ErrElevationCancelled = errors.New("elevation cancelled by user")

// Entry represents a single line in the IntraFlow-managed hosts zone. The
// line is rendered as "<IP> <Host>". IntraFlow always uses IP="127.0.0.1".
type Entry struct {
	IP   string
	Host string
}

// String renders the entry as a hosts-file line: "<IP> <Host>".
func (e Entry) String() string {
	return e.IP + " " + e.Host
}

// HostsPath returns the absolute path to the system hosts file for the current
// operating system. On darwin and linux this is "/etc/hosts"; on windows it is
// "C:\Windows\System32\drivers\etc\hosts".
func HostsPath() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Windows\System32\drivers\etc\hosts`
	default:
		// darwin, linux, and any other unix-like systems.
		return "/etc/hosts"
	}
}

// ParseZone splits the full hosts file content into three parts relative to
// the IntraFlow marker zone: the text before the begin marker, the parsed
// entries between the markers, and the text after the end marker.
//
// Entries are parsed from every non-empty, non-comment line between the
// markers. A line is treated as an entry by splitting on whitespace into at
// most two fields (IP and Host); lines that do not contain at least two fields
// are skipped.
//
// If no marker zone is present, before is set to the original content, after
// is empty, entries is nil, and hasZone is false.
func ParseZone(content string) (before string, entries []Entry, after string, hasZone bool) {
	beginIdx := strings.Index(content, BeginMarker)
	if beginIdx < 0 {
		return content, nil, "", false
	}

	before = content[:beginIdx]

	rest := content[beginIdx+len(BeginMarker):]
	// Skip the newline immediately following the begin marker.
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r\n")

	endIdx := strings.Index(rest, EndMarker)
	var zoneBody string
	if endIdx < 0 {
		// No end marker: treat the remainder as zone body with no trailing text.
		zoneBody = rest
		after = ""
	} else {
		zoneBody = rest[:endIdx]
		tail := rest[endIdx+len(EndMarker):]
		tail = strings.TrimPrefix(tail, "\n")
		tail = strings.TrimPrefix(tail, "\r\n")
		after = tail
	}

	for _, line := range strings.Split(zoneBody, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		entries = append(entries, Entry{IP: fields[0], Host: fields[1]})
	}

	return before, entries, after, true
}

// BuildZone renders the full IntraFlow marker block for the given entries:
// BeginMarker, one line per entry, then EndMarker, each terminated by "\n".
func BuildZone(entries []Entry) string {
	var b strings.Builder
	b.WriteString(BeginMarker)
	b.WriteString("\n")
	for _, e := range entries {
		b.WriteString(e.String())
		b.WriteString("\n")
	}
	b.WriteString(EndMarker)
	b.WriteString("\n")
	return b.String()
}

// ReplaceZone returns newContent with the IntraFlow marker zone replaced by a
// freshly built zone containing entries. Everything outside the zone is
// preserved verbatim.
//
// If no zone is present, the new zone is appended at the end of content. A
// blank line is inserted before the zone unless the existing content already
// ends with a blank line (i.e. ends with "\n\n" or is empty/whitespace-only
// ending in a newline).
func ReplaceZone(content string, entries []Entry) string {
	before, _, after, hasZone := ParseZone(content)
	zone := BuildZone(entries)

	if !hasZone {
		// Insert a fresh zone at the end of the file.
		if content == "" {
			return zone
		}
		// Ensure a blank line separates existing content from the zone.
		if strings.HasSuffix(content, "\n\n") || content == "\n" {
			return content + zone
		}
		if strings.HasSuffix(content, "\n") {
			return content + "\n" + zone
		}
		return content + "\n\n" + zone
	}

	// Reassemble: before + zone + after, preserving the original trailing
	// content verbatim. We preserve a single newline boundary between the
	// before-text and the begin marker only if before is non-empty and does
	// not already end with a newline.
	if before != "" && !strings.HasSuffix(before, "\n") {
		before += "\n"
	}
	return before + zone + after
}

// Diff describes the consistency state between the IntraFlow config and the
// hosts file marker zone.
//
// MissingInHosts lists domains enabled in the config but absent from the hosts
// zone. MissingInConfig lists domains present in the hosts zone but not in the
// config (i.e. residuals from previously-removed records). Consistent is true
// only when both slices are empty.
type Diff struct {
	MissingInHosts []string
	MissingInConfig []string
	Consistent     bool
}

// CheckConsistency compares the enabled config domains against the entries in
// the IntraFlow marker zone of hostsContent and reports the differences.
//
// configDomains is the list of public domains from enabled records. The hosts
// zone is parsed from hostsContent via ParseZone; only entries whose IP is
// "127.0.0.1" are considered, but all host names in the zone are compared
// regardless of IP to surface residuals.
func CheckConsistency(configDomains []string, hostsContent string) Diff {
	_, zoneEntries, _, _ := ParseZone(hostsContent)

	configSet := make(map[string]struct{}, len(configDomains))
	for _, d := range configDomains {
		configSet[d] = struct{}{}
	}

	zoneSet := make(map[string]struct{}, len(zoneEntries))
	for _, e := range zoneEntries {
		zoneSet[e.Host] = struct{}{}
	}

	diff := Diff{Consistent: true}

	for d := range configSet {
		if _, ok := zoneSet[d]; !ok {
			diff.MissingInHosts = append(diff.MissingInHosts, d)
			diff.Consistent = false
		}
	}
	for h := range zoneSet {
		if _, ok := configSet[h]; !ok {
			diff.MissingInConfig = append(diff.MissingInConfig, h)
			diff.Consistent = false
		}
	}

	// Deterministic ordering for stable test/output behavior.
	sortStrings(diff.MissingInHosts)
	sortStrings(diff.MissingInConfig)
	return diff
}

func sortStrings(s []string) {
	// Simple insertion sort to keep the stdlib-only constraint minimal; input
	// sizes are tiny (a handful of domains). Avoids pulling in "sort" for no
	// real benefit, though "sort" is stdlib. Prefer clarity here.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// buildElevateCommand constructs the privileged command used to copy the
// contents of sourcePath (a temp file written by the current user) onto
// targetPath (the system hosts file). It returns the executable name and its
// arguments suitable for os/exec.Command.
//
// On darwin the command is run via `osascript -e 'do shell script "..." with
// administrator privileges'`, which triggers a native macOS admin-password
// dialog WITHOUT requiring a controlling terminal. (Do NOT wrap osascript in
// `sudo` — sudo needs a TTY for its own password prompt, which a GUI app
// launched from a dock/icon does not have, causing a silent failure with no
// dialog at all.) On linux it prefers pkexec (a graphical polkit prompt) and
// falls back to sudo (terminal prompt). On windows it is currently a stub
// returning a not-yet-implemented error indicator via a sentinel command;
// callers should handle GOOS==windows before reaching here in production code.
func buildElevateCommand(targetPath string, sourcePath string) (name string, args []string) {
	switch runtime.GOOS {
	case "darwin":
		// Build the inner shell command: copy the temp file over the hosts
		// file, preserving its mode. Path arguments are single-quoted in the
		// shell command so NO AppleScript escaping of double quotes is
		// needed. Using double quotes here would require escaping them for
		// the AppleScript string context (\"), which triggers an AppleScript
		// syntax error ("unknown token after identifier") on non-English
		// system locales. Single quotes are safe in both shell and
		// AppleScript string contexts.
		inner := fmt.Sprintf(`/bin/cp '%s' '%s'`, sourcePath, targetPath)
		script := fmt.Sprintf(`do shell script "%s" with administrator privileges`, inner)
		return "osascript", []string{"-e", script}
	case "linux":
		// Prefer pkexec for a graphical prompt; callers may fall back to sudo
		// if pkexec is not present on PATH. Single-quote paths for shell safety.
		inner := fmt.Sprintf(`/bin/cp '%s' '%s'`, sourcePath, targetPath)
		return "pkexec", []string{"sh", "-c", inner}
	case "windows":
		// Windows elevation is handled via PowerShell Start-Process -Verb RunAs
		// in a full implementation. For the MVP we return a no-op command; the
		// caller returns ErrNotImplementedWindows before exec.
		return "powershell", []string{"-NoProfile", "-Command", "exit 0"}
	default:
		inner := fmt.Sprintf(`/bin/cp '%s' '%s'`, sourcePath, targetPath)
		return "sudo", []string{"sh", "-c", inner}
	}
}

// ErrNotImplementedWindows is returned by WriteHostsElevated on Windows for
// the MVP. A full PowerShell-based runas implementation is planned but not
// yet wired up.
var ErrNotImplementedWindows = errors.New("windows elevated write not yet implemented")

// WriteHostsElevated writes newContent to the system hosts file with
// administrator privileges. It first stages newContent in a temp file owned
// by the current user, then invokes the platform's native elevation mechanism
// to copy that temp file over the hosts file, and finally removes the temp
// file.
//
// On darwin it uses `sudo osascript ... with administrator privileges`,
// producing a native macOS password dialog. On linux it tries pkexec and
// falls back to sudo (which prompts in the controlling terminal; this is a
// known limitation for non-graphical contexts). On windows it currently
// returns ErrNotImplementedWindows.
//
// If the user cancels the elevation prompt, WriteHostsElevated returns
// ErrElevationCancelled (use IsCancellation to test).
func WriteHostsElevated(newContent string) error {
	if runtime.GOOS == "windows" {
		return ErrNotImplementedWindows
	}

	targetPath := HostsPath()

	tmp, err := os.CreateTemp("", "intraflow-hosts-*.txt")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure cleanup regardless of outcome.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(newContent); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// On linux, prefer pkexec but fall back to sudo if pkexec is unavailable.
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("pkexec"); err != nil {
			inner := fmt.Sprintf(`/bin/cp '%s' '%s'`, tmpPath, targetPath)
			return runElevated("sudo", []string{"sh", "-c", inner})
		}
	}

	name, args := buildElevateCommand(targetPath, tmpPath)
	return runElevated(name, args)
}

// runElevated executes the privileged command and translates cancellation
// signals into ErrElevationCancelled.
func runElevated(name string, args []string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		low := strings.ToLower(stderr.String())
		// Log the full command + output for debugging elevation failures
		// (the osascript path is notoriously environment-sensitive).
		log.Printf("hosts: elevated write failed: cmd=%s %v exit=%v stderr=%q stdout=%q",
			name, args, err, strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
		// osascript reports user cancellation with exit status 1 and a message
		// like "User canceled" / "user canceled".
		if strings.Contains(low, "user canceled") || strings.Contains(low, "user cancelled") {
			return fmt.Errorf("%w: %s", ErrElevationCancelled, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("elevated write failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// IsCancellation reports whether err is or wraps ErrElevationCancelled.
func IsCancellation(err error) bool {
	return errors.Is(err, ErrElevationCancelled)
}