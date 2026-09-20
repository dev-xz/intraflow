package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// ErrAlreadyRunning is returned by AcquireLock when another IntraFlow instance
// already holds the single-instance lock.
var ErrAlreadyRunning = errors.New("intraflow is already running")

// singleInstancePort is the fixed localhost TCP port used as the single-
// instance lock on Windows (where unix-domain sockets are not a stdlib
// primitive). The value is an arbitrary high port in the dynamic range;
// it is documented here so a future change is deliberate.
const singleInstancePort = 58731

// AcquireLock acquires the single-instance lock. On success it returns a
// release function that closes the underlying listener and (on unix) removes
// the socket file. On failure because another instance holds the lock, the
// returned error is ErrAlreadyRunning.
//
//   - darwin/linux: a unix-domain socket is bound at
//     ~/.intraflow/intraflow.lock. The parent dir is created (0700) if
//     missing. A stale socket file (from a crashed previous run) is removed
//     before binding so a crash does not permanently wedge the lock.
//   - windows: a TCP listener is bound on 127.0.0.1:singleInstancePort. A
//     concurrent instance fails to bind the same port, which we map to
//     ErrAlreadyRunning.
func AcquireLock() (func(), error) {
	switch runtime.GOOS {
	case "windows":
		return acquireLockWindows()
	default:
		return acquireLockUnix()
	}
}

// lockDir returns ~/.intraflow, creating it if missing. os.UserHomeDir
// respects HOME (testable via t.Setenv).
func lockDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("single-instance: resolve home: %w", err)
	}
	dir := filepath.Join(home, ".intraflow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("single-instance: create lock dir: %w", err)
	}
	return dir, nil
}

func acquireLockUnix() (func(), error) {
	dir, err := lockDir()
	if err != nil {
		return nil, err
	}
	sockPath := filepath.Join(dir, "intraflow.lock")

	// Remove a stale socket from a previous crashed run. If the file exists
	// and is a live socket owned by another instance, the bind below will
	// fail with EADDRINUSE and we report ErrAlreadyRunning. If it is stale
	// (no listener), the remove succeeds and we bind fresh.
	if _, err := os.Stat(sockPath); err == nil {
		// Try to connect: if a server is listening, the lock is held.
		c, dialErr := net.Dial("unix", sockPath)
		if dialErr == nil {
			_ = c.Close()
			return nil, ErrAlreadyRunning
		}
		// No listener -> stale file. Remove it.
		_ = os.Remove(sockPath)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
	}
	release := func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}
	return release, nil
}

func acquireLockWindows() (func(), error) {
	addr := fmt.Sprintf("127.0.0.1:%d", singleInstancePort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
	}
	release := func() {
		_ = ln.Close()
	}
	return release, nil
}