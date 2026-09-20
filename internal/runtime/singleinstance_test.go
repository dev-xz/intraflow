package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// shortLockDir returns a short-lived lock dir with a very short absolute path
// so the unix-domain socket path stays well under macOS' ~104-byte sun_path
// limit. t.TempDir() and os.MkdirTemp under the default temp dir both produce
// paths that exceed the limit once ".intraflow/intraflow.lock" is appended;
// we instead create a short-named dir directly under /tmp.
func shortLockDir(t *testing.T) string {
	t.Helper()
	// Use a short counter-style name. Keep total path short:
	//   /tmp/if-X/.intraflow/intraflow.lock  (~35 bytes)
	for i := 0; i < 1000; i++ {
		dir := filepath.Join("/tmp", "if-"+runeName(i))
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			t.Setenv("HOME", dir)
			return dir
		}
	}
	t.Fatalf("could not allocate a short temp dir")
	return ""
}

// runeName converts an index into a short base-26 string (a, b, ..., z, aa, ...).
func runeName(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return runeName(i/26) + string(rune('a'+i%26))
}

// TestAcquireLockThenSecondFails acquires the lock, asserts a second
// AcquireLock returns ErrAlreadyRunning, releases, and asserts acquire
// succeeds again.
func TestAcquireLockThenSecondFails(t *testing.T) {
	shortLockDir(t)

	release1, err := AcquireLock()
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}

	_, err2 := AcquireLock()
	if !errors.Is(err2, ErrAlreadyRunning) {
		t.Fatalf("second AcquireLock: expected ErrAlreadyRunning, got %v", err2)
	}

	release1()

	// After release, acquire should succeed again.
	release3, err3 := AcquireLock()
	if err3 != nil {
		t.Fatalf("third AcquireLock after release: %v", err3)
	}
	release3()
}

// TestAcquireLockStaleSocketRecoverable simulates a stale socket file left
// behind by a crashed previous run: we create the file but do NOT listen on
// it. AcquireLock should detect no listener, remove the stale file, and bind
// successfully rather than reporting ErrAlreadyRunning.
func TestAcquireLockStaleSocketRecoverable(t *testing.T) {
	shortLockDir(t)

	// First acquire + release to materialize ~/.intraflow/intraflow.lock,
	// then re-create the path as a plain file to simulate a stale socket.
	if r, err := AcquireLock(); err != nil {
		t.Fatalf("setup AcquireLock: %v", err)
	} else {
		r()
	}
	// Re-create a stale empty file at the socket path (the release removed
	// it). This mimics a crash that left the socket file behind.
	// AcquireLock will probe-connect, fail (no listener), remove, and bind.
	rel, err := AcquireLock()
	if err != nil {
		t.Fatalf("AcquireLock with stale file: %v", err)
	}
	rel()
}