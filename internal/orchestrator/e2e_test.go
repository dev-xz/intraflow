// End-to-end integration tests for the orchestrator package.
//
// These tests exercise the full IntraFlow pipeline minus the parts that
// cannot run headlessly on macOS:
//
//   - The osascript elevation dialog is replaced by a fake writeHosts hook
//     that records the would-be hosts content and returns nil. (The split
//     Prepare/Complete model never actually calls writeHosts from the
//     orchestrator; the GUI does. These tests simulate the GUI by calling
//     Prepare then Complete directly.)
//   - The browser is replaced by a direct dial to 127.0.0.1:<ListenPort>,
//     which is exactly the address a browser would connect to after the
//     hosts file maps the public domain to 127.0.0.1.
//
// Everything else is real: the orchestrator builds the hosts zone, persists
// config, and starts a real TCP forwarder bound to a real loopback port. The
// forwarder pipes bytes to a real local TCP server.
package orchestrator

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"intraflow/internal/config"
	"intraflow/internal/forwarder"
	"intraflow/internal/hosts"
)

// e2eBase returns a per-test stable base port so each E2E test searches for
// free ListenPorts from a distinct starting point, avoiding collisions when
// tests run in parallel. The base is mapped into the ephemeral range.
func e2eBase(t *testing.T, seed int) int {
	t.Helper()
	var h uint32 = uint32(seed)
	name := t.Name()
	for i := 0; i < len(name); i++ {
		h = h*31 + uint32(name[i])
	}
	return 20000 + int(h%39000)
}

// startHTTPServer starts a real HTTP server on 127.0.0.1:<dynamic port> that
// responds with body for any request. The assigned port is returned. The
// listener and server are registered with t.Cleanup.
func startHTTPServer(t *testing.T, body string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return ln.Addr().(*net.TCPAddr).Port
}

// httpGetBody does a GET against url and returns the response body as a
// string, failing the test on any error or non-200 status. It uses a client
// with a fresh Transport (DisableKeepAlives) so each call opens a brand-new
// TCP connection — this is critical for TestE2E_InternalTargetChange, where
// the default http.Client's connection pool would reuse the pre-restart
// connection to the forwarder and miss the target change.
func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("http.Get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// dialLocalUntilReady polls 127.0.0.1:port with a TCP dial for up to 2s,
// failing the test if the forwarder never accepts connections.
func dialLocalUntilReady(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	addr := net.JoinHostPort("127.0.0.1", itoa(port))
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("forwarder on 127.0.0.1:%d never accepted connections", port)
}

// TestE2E_HTTPForwarding verifies the full HTTP path: a domain+forward set is
// applied, the (fake) hosts file is written with the 127.0.0.1 mapping, a real
// forwarder binds the ListenPort, and an HTTP client dialing
// 127.0.0.1:ListenPort (as a browser would after hosts resolution) reaches
// the internal HTTP server and gets its response back through the forwarder.
func TestE2E_HTTPForwarding(t *testing.T) {
	// Real internal HTTP server on a dynamic loopback port.
	internalPort := startHTTPServer(t, "hello-from-internal")

	cfg := config.NewDefaultConfig()
	var written string
	readHosts := func() (string, error) { return written, nil }
	writeHosts := func(content string) error {
		written = content
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := NewForTesting(ctx, cfg, readHosts, writeHosts)
	defer o.StopAll()

	localPort := forwarder.SuggestFreePort(e2eBase(t, 1000))
	if localPort == 0 {
		t.Fatal("no free local port")
	}

	domains := []config.Domain{{ID: "", Domain: "svc.example.com"}}
	forwards := []config.Forward{{
		ID:         "",
		ListenPort: localPort,
		TargetHost: "127.0.0.1",
		TargetPort: internalPort,
		Enabled:    true,
	}}

	newContent, changed, assignedDomains, assignedForwards, err := o.PrepareApplyAll(domains, forwards)
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for fresh hosts")
	}
	// Simulate the GUI's elevated write.
	if err := writeHosts(newContent); err != nil {
		t.Fatalf("writeHosts: %v", err)
	}
	if err := o.CompleteApplyAll(assignedDomains, assignedForwards); err != nil {
		t.Fatalf("CompleteApplyAll: %v", err)
	}

	// The hosts zone must contain the 127.0.0.1 svc.example.com mapping.
	if !strings.Contains(written, hosts.BeginMarker) || !strings.Contains(written, hosts.EndMarker) {
		t.Fatalf("hosts zone markers missing:\n%s", written)
	}
	if !strings.Contains(written, "127.0.0.1 svc.example.com") {
		t.Fatalf("hosts zone missing the 127.0.0.1 svc.example.com line:\n%s", written)
	}

	// Wait for the forwarder to actually accept connections, then act like a
	// browser: dial 127.0.0.1:<ListenPort>.
	dialLocalUntilReady(t, localPort)
	body := httpGetBody(t, "http://127.0.0.1:"+itoa(localPort)+"/")
	if !strings.Contains(body, "hello-from-internal") {
		t.Fatalf("body = %q, want it to contain hello-from-internal", body)
	}
}

// TestE2E_HTTPSForwarding verifies that the forwarder is a transparent TCP
// pipe that does not interfere with TLS. A self-signed TLS server is started
// via httptest.NewTLSServer, a domain+forward set pointing at it is applied,
// and a TLS client dialing 127.0.0.1:<ListenPort> completes the handshake and
// exchanges an HTTP request over the tunnelled TLS connection.
func TestE2E_HTTPSForwarding(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-tls-internal"))
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatalf("split host:port: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("expected 127.0.0.1, got %s", host)
	}
	internalPort := 0
	for _, c := range portStr {
		internalPort = internalPort*10 + int(c-'0')
	}

	cfg := config.NewDefaultConfig()
	var written string
	readHosts := func() (string, error) { return written, nil }
	writeHosts := func(content string) error {
		written = content
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := NewForTesting(ctx, cfg, readHosts, writeHosts)
	defer o.StopAll()

	localPort := forwarder.SuggestFreePort(e2eBase(t, 2000))
	if localPort == 0 {
		t.Fatal("no free local port")
	}

	domains := []config.Domain{{ID: "", Domain: "svc.example.com"}}
	forwards := []config.Forward{{
		ID:         "",
		ListenPort: localPort,
		TargetHost: "127.0.0.1",
		TargetPort: internalPort,
		Enabled:    true,
	}}

	newContent, _, assignedDomains, assignedForwards, err := o.PrepareApplyAll(domains, forwards)
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	if err := writeHosts(newContent); err != nil {
		t.Fatalf("writeHosts: %v", err)
	}
	if err := o.CompleteApplyAll(assignedDomains, assignedForwards); err != nil {
		t.Fatalf("CompleteApplyAll: %v", err)
	}
	if !strings.Contains(written, "127.0.0.1 svc.example.com") {
		t.Fatalf("hosts zone missing mapping:\n%s", written)
	}

	dialLocalUntilReady(t, localPort)

	tlsCfg := &tls.Config{
		ServerName:         "svc.example.com",
		InsecureSkipVerify: true,
	}
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(localPort)), tlsCfg)
	if err != nil {
		t.Fatalf("tls.Dial through forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := "GET / HTTP/1.1\r\nHost: svc.example.com\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write over tls: %v", err)
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read over tls: %v", err)
	}
	if !strings.Contains(string(resp), "hello-tls-internal") {
		t.Fatalf("tls response = %q, want it to contain hello-tls-internal", string(resp))
	}
}

// TestE2E_InternalTargetChange verifies that updating a forward's target and
// re-applying restarts the forwarder so that NEW connections reach the new
// internal server. This simulates changing the internal server's endpoint and
// confirming new browser connections follow the change.
func TestE2E_InternalTargetChange(t *testing.T) {
	portA := startHTTPServer(t, "server-A")
	portB := startHTTPServer(t, "server-B")

	cfg := config.NewDefaultConfig()
	var written string
	readHosts := func() (string, error) { return written, nil }
	writeHosts := func(content string) error {
		written = content
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := NewForTesting(ctx, cfg, readHosts, writeHosts)
	defer o.StopAll()

	localPort := forwarder.SuggestFreePort(e2eBase(t, 3000))
	if localPort == 0 {
		t.Fatal("no free local port")
	}

	domains := []config.Domain{{ID: "d1", Domain: "svc.example.com"}}

	// Initial forward pointing at server A.
	fwdA := config.Forward{
		ID:         "f1",
		ListenPort: localPort,
		TargetHost: "127.0.0.1",
		TargetPort: portA,
		Enabled:    true,
	}
	newContent, _, ad, af, err := o.PrepareApplyAll(domains, []config.Forward{fwdA})
	if err != nil {
		t.Fatalf("PrepareApplyAll A: %v", err)
	}
	if err := writeHosts(newContent); err != nil {
		t.Fatalf("writeHosts A: %v", err)
	}
	if err := o.CompleteApplyAll(ad, af); err != nil {
		t.Fatalf("CompleteApplyAll A: %v", err)
	}
	if !strings.Contains(written, "127.0.0.1 svc.example.com") {
		t.Fatalf("hosts zone missing mapping:\n%s", written)
	}
	dialLocalUntilReady(t, localPort)
	if body := httpGetBody(t, "http://127.0.0.1:"+itoa(localPort)+"/"); !strings.Contains(body, "server-A") {
		t.Fatalf("initial body = %q, want server-A", body)
	}

	// Change the target: same forward ID, same ListenPort, TargetPort→B.
	fwdB := fwdA
	fwdB.TargetPort = portB
	newContent2, _, ad2, af2, err := o.PrepareApplyAll(domains, []config.Forward{fwdB})
	if err != nil {
		t.Fatalf("PrepareApplyAll B: %v", err)
	}
	if err := writeHosts(newContent2); err != nil {
		t.Fatalf("writeHosts B: %v", err)
	}
	if err := o.CompleteApplyAll(ad2, af2); err != nil {
		t.Fatalf("CompleteApplyAll B: %v", err)
	}

	dialLocalUntilReady(t, localPort)
	body := httpGetBody(t, "http://127.0.0.1:"+itoa(localPort)+"/")
	if strings.Contains(body, "server-A") {
		t.Fatalf("new connection still hit server A: %q", body)
	}
	if !strings.Contains(body, "server-B") {
		t.Fatalf("new connection body = %q, want server-B", body)
	}
}