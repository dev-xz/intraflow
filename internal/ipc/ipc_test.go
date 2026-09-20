package ipc

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestIPC_RoundTrip verifies the server dispatches a method to the handler
// and the client receives the structured result (ListForwards path).
func TestIPC_RoundTrip(t *testing.T) {
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		if method != MethodListForwards {
			t.Fatalf("unexpected method %q", method)
		}
		return []ForwardStatusDTO{
			{
				Forward: ForwardDTO{ID: "abc", ListenPort: 8080, TargetHost: "nas.local", TargetPort: 8080, Enabled: true},
				Status:  StatusInfoDTO{State: "listening", ResolvedIP: "10.0.0.5"},
			},
		}, nil
	}
	srv, err := ListenAndServe(handler)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := Dial(2 * time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	forwards, err := c.ListForwards()
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	if len(forwards) != 1 || forwards[0].Forward.TargetHost != "nas.local" {
		t.Fatalf("unexpected forwards: %+v", forwards)
	}
	if forwards[0].Status.State != "listening" || forwards[0].Status.ResolvedIP != "10.0.0.5" {
		t.Fatalf("unexpected status: %+v", forwards[0].Status)
	}
}

// TestIPC_Error verifies an error from the handler propagates to the client.
func TestIPC_Error(t *testing.T) {
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		return nil, errBoom
	}
	srv, err := ListenAndServe(handler)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := Dial(2 * time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.GetSettings(); err == nil {
		t.Fatal("expected error, got nil")
	}
}

var errBoom = &testErr{"boom"}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

// TestIPC_SaveAll verifies a typed round-trip with params + structured result
// for the batch apply Prepare path.
func TestIPC_SaveAll(t *testing.T) {
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		if method != MethodPrepareApplyAll {
			t.Fatalf("unexpected method %q", method)
		}
		var p SaveAllParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if len(p.Domains) != 1 || p.Domains[0].Domain != "svc.example.com" {
			t.Fatalf("unexpected domains: %+v", p.Domains)
		}
		assigned := p.Domains
		assigned[0].ID = "newid"
		return PrepareApplyAllResult{
			HostsContent:    "# BEGIN IntraFlow\n127.0.0.1 svc.example.com\n# END IntraFlow\n",
			Changed:         true,
			AssignedDomains: assigned,
			AssignedForwards: p.Forwards,
		}, nil
	}
	srv, err := ListenAndServe(handler)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := Dial(2 * time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	prep, err := c.PrepareApplyAll(
		[]DomainDTO{{Domain: "svc.example.com"}},
		[]ForwardDTO{{ListenPort: 8080, TargetHost: "nas.local", TargetPort: 8080, Enabled: true}},
	)
	if err != nil {
		t.Fatalf("PrepareApplyAll: %v", err)
	}
	if !prep.Changed || len(prep.AssignedDomains) != 1 || prep.AssignedDomains[0].ID != "newid" {
		t.Fatalf("unexpected result: %+v", prep)
	}
}

// TestIPC_BroadcastEvent verifies a server-pushed event reaches a connected
// client's OnEvent handler, and that the event name + data round-trip intact.
// It also confirms regular request/response still works alongside events
// (the readLoop distinguishes them by probing for an `event` field).
func TestIPC_BroadcastEvent(t *testing.T) {
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		if method != MethodGetSettings {
			t.Fatalf("unexpected method %q", method)
		}
		return SettingsDTO{Paused: true, DNSRefreshMinutes: 5, AutoStartState: "enabled"}, nil
	}
	srv, err := ListenAndServe(handler)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := Dial(2 * time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Register the event handler BEFORE issuing any RPC so the broadcast
	// can't race past a not-yet-registered handler.
	var (
		mu   sync.Mutex
		name string
		data json.RawMessage
		got  = make(chan struct{}, 1)
	)
	c.OnEvent(func(n string, d json.RawMessage) {
		mu.Lock()
		name = n
		data = d
		mu.Unlock()
		select {
		case got <- struct{}{}:
		default:
		}
	})

	// Warm up with an RPC round-trip so the server's handleConn has run past
	// its conns registration before we broadcast.
	if _, err := c.GetSettings(); err != nil {
		t.Fatalf("warmup GetSettings: %v", err)
	}

	// Broadcast a recordsChanged event with a small payload.
	if err := srv.BroadcastEvent(EventRecordsChanged, map[string]int{"listening": 1, "stopped": 0}); err != nil {
		t.Fatalf("BroadcastEvent: %v", err)
	}

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("event not received within 2s")
	}
	mu.Lock()
	if name != EventRecordsChanged {
		t.Fatalf("unexpected event name: %q", name)
	}
	mu.Unlock()
	var counts struct {
		Listening int `json:"listening"`
		Stopped   int `json:"stopped"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if counts.Listening != 1 || counts.Stopped != 0 {
		t.Fatalf("unexpected event data: %+v", counts)
	}

	// Confirm regular RPC still works after the event.
	s, err := c.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings after event: %v", err)
	}
	if s.AutoStartState != "enabled" || !s.Paused || s.DNSRefreshMinutes != 5 {
		t.Fatalf("unexpected settings after event: %+v", s)
	}
}

// TestIPC_BroadcastEventNoClients verifies BroadcastEvent is a safe no-op
// (returns nil, no panic) when no client is connected.
func TestIPC_BroadcastEventNoClients(t *testing.T) {
	srv, err := ListenAndServe(func(string, json.RawMessage) (interface{}, error) { return nil, nil })
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := srv.BroadcastEvent(EventRecordsChanged, nil); err != nil {
		t.Fatalf("BroadcastEvent with no clients: %v", err)
	}
}

// TestIPC_QuitHost verifies the QuitHost method round-trips a SimpleResult and
// that the server dispatches the registered method name. It also confirms the
// EventConfirmQuit broadcast reaches a connected client (the parallel frontend
// lane relies on this contract).
func TestIPC_QuitHost(t *testing.T) {
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		if method != MethodQuitHost {
			t.Fatalf("unexpected method %q", method)
		}
		return SimpleResult{OK: true}, nil
	}
	srv, err := ListenAndServe(handler)
	if err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	c, err := Dial(2 * time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Register the confirmQuit event handler BEFORE the broadcast.
	var (
		mu   sync.Mutex
		name string
		got  = make(chan struct{}, 1)
	)
	c.OnEvent(func(n string, d json.RawMessage) {
		mu.Lock()
		name = n
		mu.Unlock()
		select {
		case got <- struct{}{}:
		default:
		}
	})

	// Warm up so the server has registered the conn.
	res, err := c.QuitHost()
	if err != nil {
		t.Fatalf("QuitHost: %v", err)
	}
	if !res.OK || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Broadcast a confirmQuit event (mirrors host.onQuit's active-hosting
	// path) and confirm the client receives it.
	if err := srv.BroadcastEvent(EventConfirmQuit, nil); err != nil {
		t.Fatalf("BroadcastEvent: %v", err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmQuit event not received within 2s")
	}
	mu.Lock()
	if name != EventConfirmQuit {
		t.Fatalf("unexpected event name: %q", name)
	}
	mu.Unlock()
}