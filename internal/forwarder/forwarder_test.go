package forwarder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers --------------------------------------------------------------

// startEchoServer starts a TCP server on 127.0.0.1:0 that echoes back any
// bytes it receives. It returns the listener (so callers can inspect
// Listener.Addr()) and a cleanup function.
func startEchoServer(t *testing.T) (net.Listener, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln, func() { _ = ln.Close() }
}

// startTaggedEchoServer starts a TCP server whose response prefixes the
// received input with the given tag (e.g. "A:"). Useful for distinguishing
// backends in per-connection resolution tests.
func startTaggedEchoServer(t *testing.T, tag string) (net.Listener, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tagged echo server listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						out := append([]byte(tag+":"), buf[:n]...)
						if _, werr := c.Write(out); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln, func() { _ = ln.Close() }
}

// dialAndExchange dials addr, writes payload, and returns the first len(payload)
// bytes of the response (with a short timeout). For tagged servers, pass a
// larger wantLen to capture the prefix.
func dialAndExchange(t *testing.T, addr string, payload []byte, wantLen int) []byte {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, wantLen)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := io.ReadFull(c, buf)
	if err != nil {
		t.Fatalf("read: %v (got %d bytes)", err, n)
	}
	return buf[:n]
}

// freeLocalPort returns a port that is currently free on 127.0.0.1. It does
// not hold the listener open, so callers should bind quickly to avoid races.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()
	return addr.Port
}

// fakeLookup returns a lookup function that cycles through the given IPs and
// counts calls via *atomic.Int32.
func fakeLookup(ips []string, callCount *int32) func(string) (string, error) {
	return func(string) (string, error) {
		idx := atomic.AddInt32(callCount, 1) - 1
		i := int(idx)
		if i >= len(ips) {
			i = len(ips) - 1
		}
		return ips[i], nil
	}
}

// fakeFailingLookup returns a lookup function that always fails and counts
// calls.
func fakeFailingLookup(callCount *int32, err error) func(string) (string, error) {
	return func(string) (string, error) {
		atomic.AddInt32(callCount, 1)
		return "", err
	}
}

// dialToSelector dials 127.0.0.1:<dialPort()> on each call, letting tests
// point connections at different backends over time.
func dialToSelector(dialPort func() int) func(network, address string) (net.Conn, error) {
	return func(network, address string) (net.Conn, error) {
		return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", dialPort()))
	}
}

// dialToAddrAlways dials the given fixed backend address regardless of the
// address argument. Used to simulate "the dial succeeded against this real
// server" without caring what the cached IP was.
func dialToAddrAlways(addr string) func(network, address string) (net.Conn, error) {
	return func(network, _ string) (net.Conn, error) {
		return net.Dial(network, addr)
	}
}

// --- 3.1 Forwarder --------------------------------------------------------

func TestForwarder_EchoAndStop(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	localPort := freeLocalPort(t)
	f := New(localPort, "127.0.0.1", echoLn.Addr().(*net.TCPAddr).Port, 0, nil)

	if got := f.LocalAddr(); got != fmt.Sprintf("127.0.0.1:%d", localPort) {
		t.Fatalf("LocalAddr = %q, want %q", got, fmt.Sprintf("127.0.0.1:%d", localPort))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := f.Status(); got != StatusListening {
		t.Fatalf("Status after Start = %v, want StatusListening", got)
	}

	payload := []byte("hello intraflow")
	resp := dialAndExchange(t, f.LocalAddr(), payload, len(payload))
	if !bytes.Equal(resp, payload) {
		t.Fatalf("echo = %q, want %q", resp, payload)
	}

	f.Stop()
	if got := f.Status(); got != StatusStopped {
		t.Fatalf("Status after Stop = %v, want StatusStopped", got)
	}
	info := f.StatusInfo()
	if info.ResolvedIP != "" {
		t.Fatalf("ResolvedIP after Stop = %q, want empty (literal-IP target)", info.ResolvedIP)
	}

	// Verify the local port was released by re-binding it.
	ln2, err := net.Listen("tcp", f.LocalAddr())
	if err != nil {
		t.Fatalf("port not released after Stop: %v", err)
	}
	_ = ln2.Close()
}

// TestForwarder_StopIsIdempotent ensures Stop can be called multiple times
// without panicking or hanging.
func TestForwarder_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	localPort := freeLocalPort(t)
	f := New(localPort, "127.0.0.1", echoLn.Addr().(*net.TCPAddr).Port, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.Stop()
	f.Stop() // must be a safe no-op
	f.Stop()

	if got := f.Status(); got != StatusStopped {
		t.Fatalf("Status = %v, want StatusStopped", got)
	}
}

// TestForwarder_ContextCancel verifies that canceling the context tears down
// the accept loop and releases the local port.
func TestForwarder_ContextCancel(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	localPort := freeLocalPort(t)
	f := New(localPort, "127.0.0.1", echoLn.Addr().(*net.TCPAddr).Port, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	// The accept loop should observe ctx.Done() and close the listener.
	// Poll briefly for port release.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if IsPortAvailable(localPort) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !IsPortAvailable(localPort) {
		t.Fatalf("local port not released after ctx cancel")
	}

	// Stop should still be safe to call after ctx cancel.
	f.Stop()
}

// --- 3.5 Status reporting -------------------------------------------------

// TestForwarder_BindError verifies that starting a Forwarder on an
// already-occupied port returns an error and that Status() reports
// StatusError.
func TestForwarder_BindError(t *testing.T) {
	t.Parallel()

	// Occupy a port first.
	localPort := freeLocalPort(t)
	occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occ.Close()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	f := New(localPort, "127.0.0.1", echoLn.Addr().(*net.TCPAddr).Port, 0, nil)
	err = f.Start(context.Background())
	if err == nil {
		t.Fatalf("Start on occupied port: expected error, got nil")
	}
	if got := f.Status(); got != StatusError {
		t.Fatalf("Status after failed bind = %v, want StatusError", got)
	}
	info := f.StatusInfo()
	if info.State != StatusError {
		t.Fatalf("StatusInfo.State = %v, want StatusError", info.State)
	}
	if info.Reason == "" {
		t.Fatalf("StatusInfo.Reason is empty; want a description of the bind failure")
	}
	// Reason should come from the underlying net.Listen error; on most
	// platforms it mentions "address already in use" or "bind". We accept
	// either.
	if !containsAny(info.Reason, "address already in use", "bind", "in use") {
		t.Fatalf("StatusInfo.Reason = %q; want a bind/address-in-use error", info.Reason)
	}

	// A failed start should not block Stop from being called.
	f.Stop()
}

func TestStatus_String(t *testing.T) {
	t.Parallel()
	cases := map[Status]string{
		StatusStopped:   "stopped",
		StatusListening: "listening",
		StatusError:     "error",
		Status(99):      "status(99)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

// --- Lane B: DNS resolution cache -----------------------------------------

// TestForwarder_StartupResolveSuccess verifies that a hostname target is
// resolved once at Start, the cache is populated, no DNS calls happen for a
// literal-IP target, and StatusInfo reports the resolved IP.
func TestForwarder_StartupResolveSuccess(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	echoAddr := echoLn.Addr().String()

	var calls int32
	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", echoLn.Addr().(*net.TCPAddr).Port, 0, dialToAddrAlways(echoAddr))
	f.withLookupIP(fakeLookup([]string{"127.0.0.1"}, &calls))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("startup lookup calls = %d, want 1", got)
	}
	info := f.StatusInfo()
	if info.State != StatusListening {
		t.Fatalf("State = %v, want listening", info.State)
	}
	if info.ResolvedIP != "127.0.0.1" {
		t.Fatalf("ResolvedIP = %q, want 127.0.0.1", info.ResolvedIP)
	}

	// An actual connection goes through the cached IP and reaches the echo
	// server.
	payload := []byte("hi")
	resp := dialAndExchange(t, f.LocalAddr(), payload, len(payload))
	if !bytes.Equal(resp, payload) {
		t.Fatalf("echo = %q, want %q", resp, payload)
	}
	// No additional lookups happened during the dial (cached IP was valid).
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("lookup calls after dial = %d, want 1", got)
	}
}

// TestForwarder_StartupResolveFailure verifies that a startup resolution
// failure puts the forwarder into StatusError with the "目标解析失败" reason,
// leaves the cache empty, and returns an error from Start.
func TestForwarder_StartupResolveFailure(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	var calls int32
	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", echoLn.Addr().(*net.TCPAddr).Port, 0, dialToAddrAlways(echoLn.Addr().String()))
	f.withLookupIP(fakeFailingLookup(&calls, errors.New("no such host")))

	err := f.Start(context.Background())
	if err == nil {
		t.Fatalf("Start: expected error, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("startup lookup calls = %d, want 1", got)
	}
	info := f.StatusInfo()
	if info.State != StatusError {
		t.Fatalf("State = %v, want StatusError", info.State)
	}
	if info.ResolvedIP != "" {
		t.Fatalf("ResolvedIP = %q, want empty on startup failure", info.ResolvedIP)
	}
	if !containsAny(info.Reason, "目标解析失败") {
		t.Fatalf("Reason = %q, want it to contain 目标解析失败", info.Reason)
	}
	// Listener must have been released.
	if !IsPortAvailable(localPort) {
		t.Fatalf("local port not released after startup resolve failure")
	}
}

// TestForwarder_LiteralIPSkipsResolution verifies that a literal-IP target
// never calls the lookup function (even though one is injected at the package
// level via withLookupIP) and that ResolvedIP stays empty.
func TestForwarder_LiteralIPSkipsResolution(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()

	var calls int32
	// Inject a failing lookup per-forward; literal-IP targets must never
	// invoke it. We intentionally do NOT swap the package-level
	// defaultLookupIP here because t.Parallel() would race with other tests
	// that rely on the real (or their own injected) resolver.
	localPort := freeLocalPort(t)
	f := New(localPort, "127.0.0.1", echoLn.Addr().(*net.TCPAddr).Port, 0, nil)
	f.withLookupIP(fakeFailingLookup(&calls, errors.New("must not be called")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("lookup calls = %d, want 0 for literal-IP target", got)
	}
	info := f.StatusInfo()
	if info.ResolvedIP != "" {
		t.Fatalf("ResolvedIP = %q, want empty for literal-IP target", info.ResolvedIP)
	}
	payload := []byte("ok")
	resp := dialAndExchange(t, f.LocalAddr(), payload, len(payload))
	if !bytes.Equal(resp, payload) {
		t.Fatalf("echo = %q, want %q", resp, payload)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("lookup calls after dial = %d, want 0", got)
	}
}

// TestForwarder_PeriodicRefresh_IPChange verifies that a periodic refresh that
// returns a new IP updates the cache and that StatusInfo reflects it.
func TestForwarder_PeriodicRefresh_IPChange(t *testing.T) {
	t.Parallel()

	aLn, aCleanup := startTaggedEchoServer(t, "A")
	defer aCleanup()
	bLn, bCleanup := startTaggedEchoServer(t, "B")
	defer bCleanup()

	aAddr := aLn.Addr().String()
	bAddr := bLn.Addr().String()

	// Map IPs to backend ports so dialing the cached IP reaches the right
	// tagged server.
	ipToAddr := map[string]string{"10.0.0.1": aAddr, "10.0.0.2": bAddr}
	var mu sync.Mutex
	dial := func(network, address string) (net.Conn, error) {
		mu.Lock()
		// address is "ip:port"; strip the port.
		host := address
		for i := 0; i < len(host); i++ {
			if host[i] == ':' {
				host = host[:i]
				break
			}
		}
		target, ok := ipToAddr[host]
		mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("unknown ip %q", host)
		}
		return net.Dial(network, target)
	}

	var calls int32
	refresh := 20 * time.Millisecond
	localPort := freeLocalPort(t)
	ips := []string{"10.0.0.1", "10.0.0.2"}
	// Lookups: first call returns 10.0.0.1, subsequent calls return 10.0.0.2.
	lookup := func(string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return ips[0], nil
		}
		return ips[1], nil
	}

	f := New(localPort, "svc.example", 80, refresh, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if got := f.StatusInfo().ResolvedIP; got != "10.0.0.1" {
		t.Fatalf("initial ResolvedIP = %q, want 10.0.0.1", got)
	}
	// First connection reaches backend A.
	resp1 := dialAndExchange(t, f.LocalAddr(), []byte("x"), len("A:x"))
	if string(resp1) != "A:x" {
		t.Fatalf("conn #1 = %q, want A:x", resp1)
	}

	// Wait for the refresh ticker to fire and flip the cache.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.StatusInfo().ResolvedIP == "10.0.0.2" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.StatusInfo().ResolvedIP; got != "10.0.0.2" {
		t.Fatalf("ResolvedIP after refresh = %q, want 10.0.0.2", got)
	}
	// Now a new connection should reach backend B.
	resp2 := dialAndExchange(t, f.LocalAddr(), []byte("y"), len("B:y"))
	if string(resp2) != "B:y" {
		t.Fatalf("conn #2 = %q, want B:y", resp2)
	}
}

// TestForwarder_PeriodicRefresh_FailureKeepsCache verifies that a refresh
// failure keeps the old cache, sets StatusError with 目标解析失败, and that
// the forwarder keeps serving on the stale IP.
func TestForwarder_PeriodicRefresh_FailureKeepsCache(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()
	echoAddr := echoLn.Addr().String()

	var mu sync.Mutex
	var fail bool
	var calls int32
	lookup := func(string) (string, error) {
		atomic.AddInt32(&calls, 1)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return "", errors.New("dns timeout")
		}
		return "127.0.0.1", nil
	}
	dial := dialToAddrAlways(echoAddr)

	refresh := 20 * time.Millisecond
	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", echoLn.Addr().(*net.TCPAddr).Port, refresh, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if got := f.StatusInfo().ResolvedIP; got != "127.0.0.1" {
		t.Fatalf("initial ResolvedIP = %q, want 127.0.0.1", got)
	}

	// Flip lookup to failing and wait for at least one refresh.
	mu.Lock()
	fail = true
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.StatusInfo().State == StatusError {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	info := f.StatusInfo()
	if info.State != StatusError {
		t.Fatalf("State = %v, want StatusError after refresh failure", info.State)
	}
	if info.ResolvedIP != "127.0.0.1" {
		t.Fatalf("ResolvedIP = %q, want stale 127.0.0.1 preserved", info.ResolvedIP)
	}
	if !containsAny(info.Reason, "目标解析失败") {
		t.Fatalf("Reason = %q, want it to contain 目标解析失败", info.Reason)
	}

	// The forwarder must keep serving on the stale cache.
	payload := []byte("still-here")
	resp := dialAndExchange(t, f.LocalAddr(), payload, len(payload))
	if !bytes.Equal(resp, payload) {
		t.Fatalf("echo on stale cache = %q, want %q", resp, payload)
	}
}

// TestForwarder_PeriodicRefresh_Recovery verifies that a successful refresh
// after a refresh failure clears the error state back to StatusListening.
func TestForwarder_PeriodicRefresh_Recovery(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()
	echoAddr := echoLn.Addr().String()

	var mu sync.Mutex
	var fail bool
	lookup := func(string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return "", errors.New("dns timeout")
		}
		return "127.0.0.1", nil
	}
	dial := dialToAddrAlways(echoAddr)

	refresh := 15 * time.Millisecond
	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", echoLn.Addr().(*net.TCPAddr).Port, refresh, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	mu.Lock()
	fail = true
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.StatusInfo().State == StatusError {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.StatusInfo().State != StatusError {
		t.Fatalf("expected StatusError before recovery")
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.StatusInfo().State == StatusListening {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.StatusInfo().State; got != StatusListening {
		t.Fatalf("State after recovery = %v, want StatusListening", got)
	}
}

// TestForwarder_DialFallback verifies that when the cached-IP dial fails, the
// forwarder re-resolves the hostname once, retries with the new IP, updates
// the cache if the IP changed, and that this counts as exactly one extra
// lookup.
func TestForwarder_DialFallback(t *testing.T) {
	t.Parallel()

	newLn, newCleanup := startTaggedEchoServer(t, "NEW")
	defer newCleanup()
	newAddr := newLn.Addr().String()

	// dial succeeds only when the address points at newAddr; everything else
	// fails. We map old IP -> failure, new IP -> newAddr.
	dial := func(network, address string) (net.Conn, error) {
		// address is "ip:port".
		host := address
		for i := 0; i < len(host); i++ {
			if host[i] == ':' {
				host = host[:i]
				break
			}
		}
		if host == "10.0.0.2" {
			return net.Dial(network, newAddr)
		}
		return nil, errors.New("connection refused")
	}

	var calls int32
	// First lookup returns the stale IP; the next (fallback) returns the new IP.
	lookup := func(string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return "10.0.0.1", nil
		}
		return "10.0.0.2", nil
	}

	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", 80, 0, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	if got := f.StatusInfo().ResolvedIP; got != "10.0.0.1" {
		t.Fatalf("initial cache = %q, want 10.0.0.1", got)
	}

	// Inbound connection: dial against 10.0.0.1 fails → fallback re-resolve
	// returns 10.0.0.2 → retry succeeds and reaches NEW backend.
	resp := dialAndExchange(t, f.LocalAddr(), []byte("z"), len("NEW:z"))
	if string(resp) != "NEW:z" {
		t.Fatalf("fallback response = %q, want NEW:z", resp)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("lookup calls = %d, want 2 (startup + fallback)", got)
	}
	if got := f.StatusInfo().ResolvedIP; got != "10.0.0.2" {
		t.Fatalf("cache after fallback = %q, want 10.0.0.2", got)
	}
}

// TestForwarder_DialFallback_RetryFails verifies that when both the cached and
// the re-resolved dials fail, the connection is dropped and the lookup ran
// exactly once more.
func TestForwarder_DialFallback_RetryFails(t *testing.T) {
	t.Parallel()

	var calls int32
	lookup := fakeLookup([]string{"10.0.0.1", "10.0.0.2"}, &calls)
	dial := func(network, address string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}

	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", 80, 0, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	c, err := net.DialTimeout("tcp", f.LocalAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte("ignored"))
	buf := make([]byte, 4)
	_, rerr := c.Read(buf)
	_ = c.Close()
	if rerr == nil {
		t.Fatalf("expected the connection to be dropped, got a response")
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("lookup calls = %d, want 2 (startup + fallback)", got)
	}
}

// TestForwarder_PerConnectionResolution proves the cached-IP model still lets
// tests inject a custom dial function. Here we drive the cached IP via
// lookups (one per dial fallback) so two connections reach two different
// backends.
func TestForwarder_PerConnectionResolution(t *testing.T) {
	t.Parallel()

	aLn, aCleanup := startTaggedEchoServer(t, "A")
	defer aCleanup()
	bLn, bCleanup := startTaggedEchoServer(t, "B")
	defer bCleanup()

	aAddr := aLn.Addr().String()
	bAddr := bLn.Addr().String()

	// Two IPs mapping to two backends.
	ipToAddr := map[string]string{"10.0.0.1": aAddr, "10.0.0.2": bAddr}
	dial := func(network, address string) (net.Conn, error) {
		host := address
		for i := 0; i < len(host); i++ {
			if host[i] == ':' {
				host = host[:i]
				break
			}
		}
		target, ok := ipToAddr[host]
		if !ok {
			return nil, fmt.Errorf("unknown ip %q", host)
		}
		return net.Dial(network, target)
	}

	// First dial succeeds (cached IP 10.0.0.1 → A). Then we flip the cache to
	// 10.0.0.2 before the second connection so it dials B without needing a
	// fallback.
	var calls int32
	lookup := func(string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return "10.0.0.1", nil
		}
		return "10.0.0.2", nil
	}

	localPort := freeLocalPort(t)
	f := New(localPort, "svc.example", 80, 0, dial)
	f.withLookupIP(lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Stop()

	resp1 := dialAndExchange(t, f.LocalAddr(), []byte("ping"), len("A:ping"))
	if string(resp1) != "A:ping" {
		t.Fatalf("conn #1 = %q, want A:ping", resp1)
	}

	// Force the cache to the second IP by triggering a fallback: close
	// backend A so dialing 10.0.0.1 fails → fallback re-resolve returns
	// 10.0.0.2 → retry reaches backend B and updates the cache.
	aCleanup()
	resp2 := dialAndExchange(t, f.LocalAddr(), []byte("ping"), len("B:ping"))
	if string(resp2) != "B:ping" {
		t.Fatalf("conn #2 = %q, want B:ping", resp2)
	}

	// Lookups: 1 startup + 1 fallback on conn #2 (conn #1 used the cache).
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("lookup calls = %d, want 2", got)
	}
}

// --- 3.3 Pool manager -----------------------------------------------------

func TestPool_StartStop(t *testing.T) {
	t.Parallel()

	aLn, aCleanup := startTaggedEchoServer(t, "A")
	defer aCleanup()
	bLn, bCleanup := startTaggedEchoServer(t, "B")
	defer bCleanup()

	pool := NewPool()
	defer pool.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	portA := freeLocalPort(t)
	portB := freeLocalPort(t)
	if portA == portB {
		portB = freeLocalPort(t)
	}

	if err := pool.Start(ctx, "recA", portA, "127.0.0.1", aLn.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatalf("pool.Start recA: %v", err)
	}
	if err := pool.Start(ctx, "recB", portB, "127.0.0.1", bLn.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatalf("pool.Start recB: %v", err)
	}

	// Both should be listening and working.
	if st, ok := pool.Status("recA"); !ok || st != StatusListening {
		t.Fatalf("recA status = (%v,%v), want (listening,true)", st, ok)
	}
	if st, ok := pool.Status("recB"); !ok || st != StatusListening {
		t.Fatalf("recB status = (%v,%v), want (listening,true)", st, ok)
	}

	respA := dialAndExchange(t, fmt.Sprintf("127.0.0.1:%d", portA), []byte("x"), len("A:x"))
	if string(respA) != "A:x" {
		t.Fatalf("recA response = %q, want %q", respA, "A:x")
	}
	respB := dialAndExchange(t, fmt.Sprintf("127.0.0.1:%d", portB), []byte("y"), len("B:y"))
	if string(respB) != "B:y" {
		t.Fatalf("recB response = %q, want %q", respB, "B:y")
	}

	// Stop recA; recB must keep working and recA's port must be released.
	pool.Stop("recA")
	if _, ok := pool.Status("recA"); ok {
		t.Fatalf("recA still present after Stop")
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portA)); err != nil {
		t.Fatalf("recA port not released: %v", err)
	} else {
		_ = ln.Close()
	}

	// recB still works.
	respB2 := dialAndExchange(t, fmt.Sprintf("127.0.0.1:%d", portB), []byte("z"), len("B:z"))
	if string(respB2) != "B:z" {
		t.Fatalf("recB response after recA stop = %q, want %q", respB2, "B:z")
	}

	// StopAll releases everything.
	pool.StopAll()
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portB)); err != nil {
		t.Fatalf("recB port not released after StopAll: %v", err)
	} else {
		_ = ln.Close()
	}
	if got := len(pool.Statuses()); got != 0 {
		t.Fatalf("Statuses after StopAll = %d entries, want 0", got)
	}
}

// TestPool_ReplaceExisting verifies that Start with an existing id stops the
// old forwarder and replaces it (and releases the old port).
func TestPool_ReplaceExisting(t *testing.T) {
	t.Parallel()

	aLn, aCleanup := startTaggedEchoServer(t, "A")
	defer aCleanup()
	bLn, bCleanup := startTaggedEchoServer(t, "B")
	defer bCleanup()

	pool := NewPool()
	defer pool.StopAll()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	portA := freeLocalPort(t)
	portB := freeLocalPort(t)
	if portA == portB {
		portB = freeLocalPort(t)
	}

	if err := pool.Start(ctx, "rec", portA, "127.0.0.1", aLn.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	resp := dialAndExchange(t, fmt.Sprintf("127.0.0.1:%d", portA), []byte("x"), len("A:x"))
	if string(resp) != "A:x" {
		t.Fatalf("response before replace = %q, want A:x", resp)
	}

	// Replace with a forwarder on a new local port pointing at B.
	if err := pool.Start(ctx, "rec", portB, "127.0.0.1", bLn.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatalf("Start #2 (replace): %v", err)
	}

	// Old port must be released.
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portA)); err != nil {
		t.Fatalf("old port not released after replace: %v", err)
	} else {
		_ = ln.Close()
	}

	// New port now serves B.
	resp2 := dialAndExchange(t, fmt.Sprintf("127.0.0.1:%d", portB), []byte("y"), len("B:y"))
	if string(resp2) != "B:y" {
		t.Fatalf("response after replace = %q, want B:y", resp2)
	}
}

// TestPool_StatusesSnapshot verifies Statuses returns a current StatusInfo
// snapshot (with ResolvedIP) and is safe to mutate.
func TestPool_StatusesSnapshot(t *testing.T) {
	t.Parallel()

	echoLn, echoCleanup := startEchoServer(t)
	defer echoCleanup()
	echoAddr := echoLn.Addr().String()

	pool := NewPool()
	defer pool.StopAll()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p1 := freeLocalPort(t)
	var calls int32
	// Build the Forwarder directly (same package) so we can inject the
	// lookup BEFORE Start, avoiding real DNS. Hostname target → ResolvedIP
	// gets populated.
	f := New(p1, "svc.example", echoLn.Addr().(*net.TCPAddr).Port, 0, dialToAddrAlways(echoAddr))
	f.withLookupIP(fakeLookup([]string{"127.0.0.1"}, &calls))
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start r1: %v", err)
	}
	pool.mu.Lock()
	pool.forwarders["r1"] = f
	pool.mu.Unlock()

	snap := pool.Statuses()
	si, ok := snap["r1"]
	if !ok {
		t.Fatalf("snapshot missing r1")
	}
	if si.State != StatusListening {
		t.Fatalf("snapshot r1 state = %v, want listening", si.State)
	}
	if si.ResolvedIP != "127.0.0.1" {
		t.Fatalf("snapshot r1 ResolvedIP = %q, want 127.0.0.1", si.ResolvedIP)
	}
	// Mutating the snapshot must not affect the pool.
	snap["r1"] = StatusInfo{State: StatusStopped}
	delete(snap, "r1")
	snap["injected"] = StatusInfo{State: StatusError}

	if got, _ := pool.Status("r1"); got != StatusListening {
		t.Fatalf("pool r1 affected by snapshot mutation: %v", got)
	}
	if _, ok := pool.Status("injected"); ok {
		t.Fatalf("pool picked up injected key from snapshot")
	}
}

// --- 3.4 Port-occupancy precheck ------------------------------------------

func TestIsPortAvailable_AndSuggestFreePort(t *testing.T) {
	t.Parallel()

	p := SuggestFreePort(1024)
	if p == 0 {
		t.Fatalf("SuggestFreePort(1024) returned 0; expected a free port")
	}
	if !IsPortAvailable(p) {
		t.Fatalf("IsPortAvailable(%d) = false, want true", p)
	}

	// Occupy p with a real listener.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		t.Fatalf("occupy %d: %v", p, err)
	}
	defer ln.Close()

	if IsPortAvailable(p) {
		t.Fatalf("IsPortAvailable(%d) = true after occupy, want false", p)
	}

	other := SuggestFreePort(p)
	if other == 0 {
		t.Fatalf("SuggestFreePort(%d) returned 0", p)
	}
	if other == p {
		t.Fatalf("SuggestFreePort(%d) returned the occupied port %d again", p, other)
	}
	if !IsPortAvailable(other) {
		t.Fatalf("SuggestFreePort(%d) returned %d which is not actually free", p, other)
	}
}

// --- misc helpers ---------------------------------------------------------

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}