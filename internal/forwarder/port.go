package forwarder

import (
	"fmt"
	"net"
)

// IsPortAvailable reports whether a TCP listener can be bound on
// 127.0.0.1:<port>. It binds and immediately closes a listener to test the
// port, returning true on success and false on any error.
func IsPortAvailable(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// SuggestFreePort returns the first available TCP port in the range [from,
// 65535]. If none is available in that range, it scans downward from 1024
// (i.e. [1024, from)) as a fallback. It returns 0 if no free port can be
// found, which is practically unreachable on a healthy host.
func SuggestFreePort(from int) int {
	if from < 0 {
		from = 1024
	}
	for p := from; p <= 65535; p++ {
		if IsPortAvailable(p) {
			return p
		}
	}
	for p := 1024; p < from; p++ {
		if IsPortAvailable(p) {
			return p
		}
	}
	return 0
}