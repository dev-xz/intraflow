// Package forwarder implements a TCP port-forwarding layer for IntraFlow.
//
// A Forwarder listens on a local loopback port and pipes each inbound TCP
// connection to an internal host:port as a transparent byte stream. No
// protocol parsing or TLS decryption is performed; bytes are copied in both
// directions with io.Copy, so end-to-end TLS (HTTPS) passes through
// untouched.
//
// DNS resolution model (Lane B / OpenSpec split-domain-forward-model):
//   - When the target host is a HOSTNAME (not a literal IP), it is resolved
//     once at Start time and the resulting IP is cached. Startup resolution
//     failure puts the forwarder into StatusError ("目标解析失败") and the
//     listener is NOT started with an empty cache.
//   - A periodic ticker (refreshEvery > 0) re-resolves the hostname. On a
//     successful IP change, the cache is updated. On refresh failure, the old
//     cache is kept and the state becomes StatusError (the forwarder keeps
//     serving on the stale cache).
//   - The dial path dials the cached IP. On dial failure for a hostname
//     target, the hostname is re-resolved once and the dial retried with the
//     fresh result; if the IP changed, the cache is updated.
//   - Literal-IP targets skip resolution entirely and dial directly with no
//     retry.
//
// A Pool manages multiple Forwarders keyed by a string record identifier and
// is safe for concurrent use.
package forwarder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Status represents the lifecycle state of a Forwarder.
type Status int

const (
	// StatusStopped means the forwarder is not running (either never started
	// or has been stopped).
	StatusStopped Status = iota
	// StatusListening means the forwarder has bound its local listener and is
	// accepting inbound connections.
	StatusListening
	// StatusError means the forwarder failed to start, its accept loop exited
	// unexpectedly, or (for hostname targets) a periodic DNS refresh failed.
	// When a refresh fails the listener keeps serving on the stale cache; see
	// the accompanying Reason string.
	StatusError
)

// String returns a human-readable description of the status.
func (s Status) String() string {
	switch s {
	case StatusStopped:
		return "stopped"
	case StatusListening:
		return "listening"
	case StatusError:
		return "error"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// StatusInfo is a per-forward status snapshot.
type StatusInfo struct {
	// State is StatusStopped / StatusListening / StatusError.
	State Status
	// Reason is a human-readable description, e.g. "端口被占用",
	// "目标解析失败" (empty when fine).
	Reason string
	// ResolvedIP is the current cached target IP. It is empty for literal-IP
	// targets (which skip resolution), before the first successful resolve of
	// a hostname target, and when the forwarder is stopped.
	ResolvedIP string
}

// defaultLookupIP is the package-default hostname-to-IP resolver. Tests swap
// it (or inject per-Forwarder via withLookupIP) to avoid real DNS lookups.
var defaultLookupIP = func(host string) (string, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", errors.New("forwarder: DNS returned no IP addresses")
	}
	return ips[0].String(), nil
}

// Forwarder forwards inbound TCP connections from 127.0.0.1:LocalPort to
// TargetHost:TargetPort as a transparent byte pipe.
type Forwarder struct {
	localPort  int
	targetHost string
	targetPort int

	// refreshEvery is the periodic DNS re-resolve interval. <= 0 disables the
	// refresh ticker.
	refreshEvery time.Duration

	// dial is the outbound dial function. nil → net.Dial.
	dial func(network, address string) (net.Conn, error)

	// lookupIP resolves the target hostname to a single IP string. nil →
	// defaultLookupIP. Only used when isHostname is true.
	lookupIP func(host string) (string, error)

	// isHostname is true when targetHost is not a literal IP and therefore
	// needs DNS resolution.
	isHostname bool

	mu       sync.Mutex
	listener net.Listener
	state    Status
	reason   string
	// resolvedIP is the cached target IP for hostname targets. Guarded by mu.
	resolvedIP string

	// refresh ticker / loop bookkeeping. Guarded by mu for setup/teardown.
	refreshTicker *time.Ticker
	refreshStop   chan struct{}
	refreshDone   chan struct{}

	// wg tracks active per-connection goroutines so Stop can wait for them
	// to fully drain if desired. The accept loop itself is tracked via
	// doneCh.
	doneCh  chan struct{}
	stopped bool
}

// New creates a Forwarder for the given local port and internal endpoint.
//
// listenPort is the loopback port to bind; targetHost is the hostname or
// literal IP to forward to; targetPort is the target's port. refreshEvery
// controls the periodic DNS re-resolve interval for hostname targets; <= 0
// disables it. dial is the outbound dial function; pass nil to use net.Dial.
//
// The returned Forwarder is not started; call Start to bind the listener.
func New(listenPort int, targetHost string, targetPort int, refreshEvery time.Duration, dial func(network, address string) (net.Conn, error)) *Forwarder {
	f := &Forwarder{
		localPort:    listenPort,
		targetHost:   targetHost,
		targetPort:   targetPort,
		refreshEvery: refreshEvery,
		dial:         dial,
		isHostname:   net.ParseIP(targetHost) == nil,
	}
	if f.dial == nil {
		f.dial = net.Dial
	}
	return f
}

// withDial replaces the dial function used to reach the internal endpoint.
// It is intended only for tests that need to inject custom backend
// connections.
func (f *Forwarder) withDial(d func(network, address string) (net.Conn, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dial = d
}

// withLookupIP replaces the hostname resolver. It is intended only for tests
// that need to avoid real DNS lookups.
func (f *Forwarder) withLookupIP(fn func(host string) (string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupIP = fn
}

// lookup resolves the target hostname using the injected or default resolver.
// Caller must NOT hold f.mu.
func (f *Forwarder) lookup() (string, error) {
	fn := f.lookupIP
	if fn == nil {
		fn = defaultLookupIP
	}
	return fn(f.targetHost)
}

// LocalAddr returns the loopback address the forwarder binds, in the form
// "127.0.0.1:<localPort>".
func (f *Forwarder) LocalAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", f.localPort)
}

// Start binds a listener on 127.0.0.1:<localPort>, resolves the target
// hostname (if applicable), and runs the accept loop until either ctx is
// canceled or Stop is called. It returns an error if the listener cannot be
// bound or if the initial DNS resolution of a hostname target fails.
//
// Start must be called at most once per Forwarder; calling it again after the
// forwarder has been stopped returns an error.
func (f *Forwarder) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return errors.New("forwarder: already stopped")
	}
	if f.listener != nil {
		f.mu.Unlock()
		return errors.New("forwarder: already started")
	}
	addr := f.LocalAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		f.state = StatusError
		f.reason = err.Error()
		f.mu.Unlock()
		return fmt.Errorf("forwarder: bind %s: %w", addr, err)
	}
	f.listener = ln
	f.mu.Unlock()

	// Resolve the target hostname once before serving. Literal-IP targets
	// skip resolution entirely.
	if f.isHostname {
		ip, err := f.lookup()
		if err != nil {
			f.mu.Lock()
			f.state = StatusError
			f.reason = fmt.Sprintf("目标解析失败: %v", err)
			f.resolvedIP = ""
			if f.listener != nil {
				_ = f.listener.Close()
				f.listener = nil
			}
			f.mu.Unlock()
			return fmt.Errorf("forwarder: resolve %s: %w", f.targetHost, err)
		}
		f.mu.Lock()
		f.resolvedIP = ip
		f.state = StatusListening
		f.reason = ""
		f.doneCh = make(chan struct{})
		f.mu.Unlock()
	} else {
		f.mu.Lock()
		f.state = StatusListening
		f.reason = ""
		f.doneCh = make(chan struct{})
		f.mu.Unlock()
	}

	// Start the periodic refresh ticker for hostname targets when enabled.
	if f.isHostname && f.refreshEvery > 0 {
		f.startRefreshLoop(ctx)
	}

	go f.acceptLoop(ctx)
	return nil
}

// startRefreshLoop spawns a goroutine that re-resolves the target hostname on
// a ticker. On a successful IP change it updates the cache; on failure it sets
// StatusError (keeping the old cache) while the listener keeps serving. A
// subsequent successful refresh clears the error back to StatusListening.
func (f *Forwarder) startRefreshLoop(ctx context.Context) {
	f.mu.Lock()
	ticker := time.NewTicker(f.refreshEvery)
	stop := make(chan struct{})
	done := make(chan struct{})
	f.refreshTicker = ticker
	f.refreshStop = stop
	f.refreshDone = done
	f.mu.Unlock()

	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				f.refreshOnce()
			}
		}
	}()
}

// refreshOnce performs a single periodic re-resolve and updates state/cache.
func (f *Forwarder) refreshOnce() {
	ip, err := f.lookup()
	f.mu.Lock()
	if err != nil {
		// Keep the old cache; flag error but stay serving on the stale IP.
		if f.state != StatusStopped {
			f.state = StatusError
			f.reason = fmt.Sprintf("目标解析失败: %v", err)
		}
		f.mu.Unlock()
		return
	}
	if f.state != StatusStopped {
		if ip != f.resolvedIP {
			f.resolvedIP = ip
		}
		// A successful refresh clears a previous refresh-error. Only clear
		// when we were in a refresh-error state (StatusListening is the
		// normal serving state).
		if f.state == StatusError {
			f.state = StatusListening
			f.reason = ""
		}
	}
	f.mu.Unlock()
}

// acceptLoop accepts inbound connections and spawns a handler goroutine per
// connection. It exits when the listener is closed (by ctx cancellation,
// Stop, or error) and reports unexpected exits as StatusError.
func (f *Forwarder) acceptLoop(ctx context.Context) {
	defer close(f.doneCh)

	// Snapshot the listener once; it is immutable for the lifetime of this
	// loop. Stop/ctx cancel may close it concurrently, which makes Accept
	// return an error — that's the expected shutdown signal.
	f.mu.Lock()
	ln := f.listener
	f.mu.Unlock()
	if ln == nil {
		return
	}

	for {
		// Honor ctx before blocking on Accept to keep teardown prompt.
		select {
		case <-ctx.Done():
			f.markStoppedFromLoop()
			return
		default:
		}

		in, err := ln.Accept()
		if err != nil {
			f.mu.Lock()
			listenerClosed := f.stopped
			f.mu.Unlock()
			if listenerClosed {
				// Expected shutdown via Stop or ctx.
				return
			}
			// Unexpected accept error.
			f.mu.Lock()
			f.state = StatusError
			f.reason = err.Error()
			if f.listener != nil {
				_ = f.listener.Close()
				f.listener = nil
			}
			f.mu.Unlock()
			return
		}

		go f.handle(ctx, in)
	}
}

// markStoppedFromLoop transitions to StatusStopped when the accept loop
// observes ctx cancellation. It closes the listener if still open and clears
// the stored reference. It does not flip f.stopped (Stop owns that flag), but
// does set the visible state to stopped.
func (f *Forwarder) markStoppedFromLoop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener = nil
	}
	if !f.stopped {
		f.state = StatusStopped
		f.reason = ""
		f.resolvedIP = ""
	}
}

// handle pipes a single inbound connection to a freshly dialed backend.
//
// For hostname targets it dials the cached IP; on dial failure it re-resolves
// once and retries with the fresh IP (updating the cache if it changed). For
// literal-IP targets it dials directly with no retry.
func (f *Forwarder) handle(ctx context.Context, in net.Conn) {
	select {
	case <-ctx.Done():
		_ = in.Close()
		return
	default:
	}

	f.mu.Lock()
	dial := f.dial
	isHostname := f.isHostname
	cachedIP := f.resolvedIP
	host := f.targetHost
	port := f.targetPort
	f.mu.Unlock()

	var out net.Conn
	var err error
	if !isHostname {
		// Literal IP: dial directly, no retry.
		out, err = dial("tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			_ = in.Close()
			return
		}
	} else {
		// Hostname: dial cached IP. On failure, re-resolve and retry once.
		if cachedIP == "" {
			// Defensive: no cache yet. Should not happen after a successful
			// Start, but resolve now rather than dialing an empty address.
			ip, lerr := f.lookup()
			if lerr != nil {
				_ = in.Close()
				return
			}
			cachedIP = ip
			f.mu.Lock()
			f.resolvedIP = ip
			f.mu.Unlock()
		}
		out, err = dial("tcp", fmt.Sprintf("%s:%d", cachedIP, port))
		if err == nil {
			pipe(in, out)
			return
		}
		// Dial failed: re-resolve and retry once with the fresh IP.
		newIP, lerr := f.lookup()
		if lerr != nil {
			_ = in.Close()
			return
		}
		f.mu.Lock()
		if newIP != f.resolvedIP {
			f.resolvedIP = newIP
		}
		f.mu.Unlock()
		out, err = dial("tcp", fmt.Sprintf("%s:%d", newIP, port))
		if err != nil {
			_ = in.Close()
			return
		}
	}

	pipe(in, out)
}

// pipe copies bytes between two connections in both directions and closes
// both when either side finishes (EOF or error). When one direction
// completes, the other side's deadline is set to the past to unblock any
// pending read/write so the second goroutine returns promptly.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.SetDeadline(time.Now())
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.SetDeadline(time.Now())
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

// Stop closes the listener, stops any refresh ticker, and signals the accept
// loop to exit. It is safe to call once; subsequent calls are no-ops. Stop
// releases the local port.
func (f *Forwarder) Stop() {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return
	}
	f.stopped = true
	f.state = StatusStopped
	f.reason = ""
	f.resolvedIP = ""
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener = nil
	}
	doneCh := f.doneCh
	var refreshStop chan struct{}
	var refreshDone chan struct{}
	if f.refreshTicker != nil {
		f.refreshTicker.Stop()
		f.refreshTicker = nil
		refreshStop = f.refreshStop
		f.refreshStop = nil
		refreshDone = f.refreshDone
		f.refreshDone = nil
	}
	f.mu.Unlock()

	if doneCh != nil {
		<-doneCh
	}
	if refreshStop != nil {
		close(refreshStop)
	}
	if refreshDone != nil {
		<-refreshDone
	}
}

// Status returns the current state of the forwarder.
func (f *Forwarder) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// StatusInfo returns the current state along with a reason string (populated
// when the state is StatusError) and the cached resolved target IP (empty for
// literal-IP targets and when stopped).
func (f *Forwarder) StatusInfo() StatusInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return StatusInfo{State: f.state, Reason: f.reason, ResolvedIP: f.resolvedIP}
}

// Pool manages a set of Forwarders keyed by a string record identifier. It is
// safe for concurrent use.
type Pool struct {
	mu         sync.RWMutex
	forwarders map[string]*Forwarder
}

// NewPool returns an empty Pool.
func NewPool() *Pool {
	return &Pool{forwarders: make(map[string]*Forwarder)}
}

// Start stops any existing forwarder for id, then creates and starts a new
// one bound to localPort that forwards to internalHost:internalPort. It
// returns an error if the new forwarder fails to start.
//
// Note: this signature keeps the pre-Lane-B shape (no refreshEvery) so
// existing callers continue to compile. Periodic DNS refresh is disabled
// (refreshEvery=0). Lane C rewires callers to pass settings.DNSRefreshMinutes.
func (p *Pool) Start(ctx context.Context, id string, localPort int, internalHost string, internalPort int) error {
	return p.StartWithRefresh(ctx, id, localPort, internalHost, internalPort, 0, nil)
}

// StartWithRefresh is the extended constructor entry: like Start but also
// passes a DNS refresh interval and an optional outbound dial function (nil →
// net.Dial). It is the primary constructor Lane C wires up.
func (p *Pool) StartWithRefresh(ctx context.Context, id string, localPort int, internalHost string, internalPort int, refreshEvery time.Duration, dial func(network, address string) (net.Conn, error)) error {
	p.stopLocked(id, false)

	f := New(localPort, internalHost, internalPort, refreshEvery, dial)
	if err := f.Start(ctx); err != nil {
		p.mu.Lock()
		// Preserve the failed forwarder so its StatusError is queryable, but
		// only if no other goroutine raced in.
		if existing, ok := p.forwarders[id]; !ok || existing == nil {
			p.forwarders[id] = f
		}
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	p.forwarders[id] = f
	p.mu.Unlock()
	return nil
}

// Stop stops and removes the forwarder for id. It is a no-op if no
// forwarder exists for id.
func (p *Pool) Stop(id string) {
	p.stopLocked(id, true)
}

// stopLocked stops the forwarder for id and, if remove is true, drops it from
// the map. Caller may hold the lock or not; this function acquires it.
func (p *Pool) stopLocked(id string, remove bool) {
	p.mu.Lock()
	f, ok := p.forwarders[id]
	if remove && ok {
		delete(p.forwarders, id)
	}
	p.mu.Unlock()
	if ok {
		f.Stop()
	}
}

// StopAll stops and removes every forwarder in the pool.
func (p *Pool) StopAll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.forwarders))
	for id := range p.forwarders {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.Stop(id)
	}
}

// Status returns the status of the forwarder for id and whether one exists.
func (p *Pool) Status(id string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	f, ok := p.forwarders[id]
	if !ok {
		return StatusStopped, false
	}
	return f.Status(), true
}

// Statuses returns a snapshot of the current status (including resolved IP)
// of every forwarder keyed by id.
func (p *Pool) Statuses() map[string]StatusInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]StatusInfo, len(p.forwarders))
	for id, f := range p.forwarders {
		out[id] = f.StatusInfo()
	}
	return out
}