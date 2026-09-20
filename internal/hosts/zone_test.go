package hosts

import (
	"runtime"
	"strings"
	"testing"
)

// TestHostsPath_ByGOOS asserts HostsPath returns the platform-correct path
// for the GOOS the test is actually running under. The windows branch
// ("C:\Windows\System32\drivers\etc\hosts") is exercised on Windows CI.
func TestHostsPath_ByGOOS(t *testing.T) {
	got := HostsPath()
	var want string
	switch runtime.GOOS {
	case "windows":
		want = `C:\Windows\System32\drivers\etc\hosts`
	default: // darwin, linux, and other unix-likes
		want = "/etc/hosts"
	}
	if got != want {
		t.Fatalf("HostsPath() = %q, want %q (GOOS=%s)", got, want, runtime.GOOS)
	}
}

func TestEntry_String(t *testing.T) {
	e := Entry{IP: "127.0.0.1", Host: "example.com"}
	if got := e.String(); got != "127.0.0.1 example.com" {
		t.Fatalf("Entry.String() = %q, want %q", got, "127.0.0.1 example.com")
	}
}

func TestBuildZone(t *testing.T) {
	entries := []Entry{
		{IP: "127.0.0.1", Host: "a.local"},
		{IP: "127.0.0.1", Host: "b.local"},
	}
	got := BuildZone(entries)
	want := BeginMarker + "\n" +
		"127.0.0.1 a.local\n" +
		"127.0.0.1 b.local\n" +
		EndMarker + "\n"
	if got != want {
		t.Fatalf("BuildZone mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestReplaceZone_NoZone_InsertsAtEnd covers case 1: ReplaceZone on content
// without a marker zone inserts the zone at the end and preserves the
// original line.
func TestReplaceZone_NoZone_InsertsAtEnd(t *testing.T) {
	original := "127.0.0.1 mycustom.local\n"
	entries := []Entry{{IP: "127.0.0.1", Host: "api.example.com"}}
	got := ReplaceZone(original, entries)

	if !strings.HasPrefix(got, original) {
		t.Fatalf("original content not preserved at start:\ngot:  %q\nwant prefix: %q", got, original)
	}
	if !strings.Contains(got, original) {
		t.Fatalf("original line not preserved verbatim: %q", got)
	}
	if !strings.Contains(got, BeginMarker) || !strings.Contains(got, EndMarker) {
		t.Fatalf("zone markers missing from result: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1 api.example.com") {
		t.Fatalf("new entry missing from result: %q", got)
	}
	// Ensure a blank line separates the original content from the zone.
	idx := strings.Index(got, BeginMarker)
	if idx <= 0 || got[idx-1] != '\n' {
		t.Fatalf("expected newline before begin marker, got: %q", got)
	}
	if idx >= 2 && got[idx-2] != '\n' {
		// there should be a blank line (two newlines) before the marker
		t.Fatalf("expected blank line before begin marker, got: %q", got)
	}
}

// TestReplaceZone_HasZone_PreservesOutside covers case 2: parse an existing
// zone, modify entries, ReplaceZone, and assert outside content is unchanged
// and the new zone matches BuildZone.
func TestReplaceZone_HasZone_PreservesOutside(t *testing.T) {
	header := "## my custom hosts\n127.0.0.1 localhost\n255.255.255.255 broadcasthost\n"
	footer := "# end of file custom note\n"
	original := header + BeginMarker + "\n" +
		"127.0.0.1 old.example.com\n" +
		EndMarker + "\n" + footer

	before, entries, after, hasZone := ParseZone(original)
	if !hasZone {
		t.Fatalf("expected zone to be detected")
	}
	if before != header {
		t.Fatalf("before mismatch:\ngot:  %q\nwant: %q", before, header)
	}
	if after != footer {
		t.Fatalf("after mismatch:\ngot:  %q\nwant: %q", after, footer)
	}
	if len(entries) != 1 || entries[0].Host != "old.example.com" {
		t.Fatalf("parsed entries mismatch: %+v", entries)
	}

	newEntries := []Entry{
		{IP: "127.0.0.1", Host: "new1.example.com"},
		{IP: "127.0.0.1", Host: "new2.example.com"},
	}
	got := ReplaceZone(original, newEntries)

	if !strings.HasPrefix(got, header) {
		t.Fatalf("header not preserved: %q", got)
	}
	if !strings.HasSuffix(got, footer) {
		t.Fatalf("footer not preserved: %q", got)
	}
	wantZone := BuildZone(newEntries)
	if !strings.Contains(got, wantZone) {
		t.Fatalf("rebuilt zone not present:\ngot: %q\nwant zone: %q", got, wantZone)
	}
	// The stale entry must be gone.
	if strings.Contains(got, "old.example.com") {
		t.Fatalf("stale entry still present after replace: %q", got)
	}
}

// TestReplaceZone_ResidualCleanup covers case 3: a stale entry in the zone
// that is not in the new entries list is removed (whole-zone replacement).
func TestReplaceZone_ResidualCleanup(t *testing.T) {
	original := BeginMarker + "\n" +
		"127.0.0.1 stale.example.com\n" +
		"127.0.0.1 keep.example.com\n" +
		EndMarker + "\n"

	newEntries := []Entry{{IP: "127.0.0.1", Host: "keep.example.com"}}
	got := ReplaceZone(original, newEntries)

	if strings.Contains(got, "stale.example.com") {
		t.Fatalf("stale entry not removed: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1 keep.example.com") {
		t.Fatalf("kept entry missing: %q", got)
	}
	wantZone := BuildZone(newEntries)
	if !strings.Contains(got, wantZone) {
		t.Fatalf("zone does not match BuildZone:\ngot: %q\nwant: %q", got, wantZone)
	}
}

// TestParseZone_ExtractsBeforeEntriesAfter covers case 4: a sample hosts file
// with hand-written content above and below the markers parses correctly.
func TestParseZone_ExtractsBeforeEntriesAfter(t *testing.T) {
	before := "# hand-written above\n127.0.0.1 localhost\n::1 localhost\n"
	zoneBody := "127.0.0.1 a.example.com\n127.0.0.1 b.example.com\n"
	after := "# hand-written below\n192.168.1.1 router.lan\n"
	content := before + BeginMarker + "\n" + zoneBody + EndMarker + "\n" + after

	gotBefore, gotEntries, gotAfter, hasZone := ParseZone(content)
	if !hasZone {
		t.Fatalf("expected zone detected")
	}
	if gotBefore != before {
		t.Fatalf("before mismatch:\ngot:  %q\nwant: %q", gotBefore, before)
	}
	if gotAfter != after {
		t.Fatalf("after mismatch:\ngot:  %q\nwant: %q", gotAfter, after)
	}
	if len(gotEntries) != 2 {
		t.Fatalf("entries count = %d, want 2: %+v", len(gotEntries), gotEntries)
	}
	if gotEntries[0].Host != "a.example.com" || gotEntries[1].Host != "b.example.com" {
		t.Fatalf("entries mismatch: %+v", gotEntries)
	}
	if gotEntries[0].IP != "127.0.0.1" {
		t.Fatalf("entry IP mismatch: %+v", gotEntries)
	}
}

func TestParseZone_NoZone(t *testing.T) {
	content := "127.0.0.1 localhost\n"
	before, entries, after, hasZone := ParseZone(content)
	if hasZone {
		t.Fatalf("expected hasZone=false")
	}
	if before != content {
		t.Fatalf("before should equal content when no zone: %q", before)
	}
	if after != "" {
		t.Fatalf("after should be empty when no zone: %q", after)
	}
	if entries != nil {
		t.Fatalf("entries should be nil when no zone: %+v", entries)
	}
}