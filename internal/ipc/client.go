// IPC client: used by the GUI process to call host methods over the unix
// socket. One persistent connection; requests multiplexed by id. Thread-safe.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a persistent connection to the IPC host. Methods are safe to call
// concurrently; each call gets a unique id and waits for its matching
// response. Server-pushed events (see Event) are dispatched to a handler
// registered via OnEvent.
type Client struct {
	conn    net.Conn
	writer  *bufio.Writer
	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int]chan Response
	closed    bool

	// eventMu guards eventFn. The handler is invoked from readLoop when a
	// line carries an Event (an `event` field) rather than a Response.
	eventMu sync.RWMutex
	eventFn func(name string, data json.RawMessage)
}

// Dial connects to the IPC host socket. It retries for up to timeout waiting
// for the host to start accepting (useful at GUI launch when the host may
// still be binding the socket).
func Dial(timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		path, err := SocketPath()
		if err != nil {
			return nil, err
		}
		conn, err := net.Dial("unix", path)
		if err == nil {
			c := &Client{
				conn:    conn,
				writer:  bufio.NewWriter(conn),
				pending: make(map[int]chan Response),
			}
			go c.readLoop()
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("ipc: dial host: %w", lastErr)
}

// readLoop drains responses and dispatches them to the waiting caller by id.
// It also dispatches server-pushed events (lines with an `event` field) to the
// handler registered via OnEvent. Distinguishing the two: a line that
// unmarshals with a non-empty `event` field is an Event; otherwise it is a
// Response (matched by `id` to a pending request).
func (c *Client) readLoop() {
	reader := bufio.NewReader(c.conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.failPending(err)
			return
		}
		// Probe: an Event has a non-empty `event` field and no client
		// request id; a Response has an `id` (and no `event`). We use a
		// pointer-typed ID so a missing `id` decodes to nil (events)
		// without losing the zero-value response id=0 (error replies
		// from malformed requests use id=0 and still match a pending
		// caller keyed by 0 — those callers get the error response).
		var probe struct {
			ID    *int   `json:"id"`
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Event != "" {
			var evt Event
			if err := json.Unmarshal(line, &evt); err != nil {
				continue
			}
			c.eventMu.RLock()
			fn := c.eventFn
			c.eventMu.RUnlock()
			if fn != nil {
				fn(evt.Name, evt.Data)
			}
			continue
		}

		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// OnEvent registers a handler invoked for every server-pushed Event received
// on this connection. Pass nil to unregister. The handler is called from the
// readLoop goroutine; it must not block (the host is the only writer, so a
// slow handler would stall response delivery for that client). Safe to call
// before or after Dial returns; the read goroutine reads eventFn under a
// read lock.
func (c *Client) OnEvent(fn func(name string, data json.RawMessage)) {
	c.eventMu.Lock()
	c.eventFn = fn
	c.eventMu.Unlock()
}

// failPending wakes all waiting callers with an error when the connection
// breaks.
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- Response{ID: id, Error: &ErrorBody{Message: fmt.Sprintf("ipc: connection lost: %v", err)}}
		delete(c.pending, id)
	}
}

// call sends a request and waits for the matching response. result is
// unmarshaled into out (which must be a pointer) on success.
func (c *Client) call(method string, params interface{}, out interface{}) error {
	c.pendingMu.Lock()
	if c.closed {
		c.pendingMu.Unlock()
		return ErrNotConnected
	}
	c.pendingMu.Unlock()

	id := int(c.nextID.Add(1))
	paramJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("ipc: marshal params: %w", err)
	}
	req := Request{ID: id, Method: method, Params: paramJSON}
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ipc: marshal request: %w", err)
	}

	ch := make(chan Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	_, werr := c.writer.Write(line)
	if werr == nil {
		werr = c.writer.WriteByte('\n')
	}
	if werr == nil {
		werr = c.writer.Flush()
	}
	c.writeMu.Unlock()
	if werr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return fmt.Errorf("ipc: write: %w", werr)
	}

	resp := <-ch
	if resp.Error != nil {
		return errors.New(resp.Error.Message)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("ipc: unmarshal result: %w", err)
		}
	}
	return nil
}

// Close terminates the connection. Further calls return ErrNotConnected.
func (c *Client) Close() error {
	c.pendingMu.Lock()
	c.closed = true
	c.pendingMu.Unlock()
	return c.conn.Close()
}

// --- Typed RPC methods ---
// Each wraps call() with the right param/result types so the GUI's App facade
// is a thin shim with no manual JSON handling.

// ListDomains returns the current domain list from the host.
func (c *Client) ListDomains() ([]DomainDTO, error) {
	var out []DomainDTO
	if err := c.call(MethodListDomains, struct{}{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListForwards returns the current forward list paired with forwarder status.
func (c *Client) ListForwards() ([]ForwardStatusDTO, error) {
	var out []ForwardStatusDTO
	if err := c.call(MethodListForwards, struct{}{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetSettings() (SettingsDTO, error) {
	var out SettingsDTO
	if err := c.call(MethodGetSettings, struct{}{}, &out); err != nil {
		return SettingsDTO{}, err
	}
	return out, nil
}

func (c *Client) SetSettings(s SettingsDTO) (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodSetSettings, SetSettingsParams{Settings: s}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// SetAutoStart toggles the OS-native login-item / LaunchAgent / Run-key
// registration. It operates directly on the OS, not on persisted preferences;
// the live result is reflected in subsequent GetSettings calls via
// SettingsDTO.AutoStartState.
func (c *Client) SetAutoStart(enabled bool) (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodSetAutoStart, SetAutoStartParams{Enabled: enabled}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// GetStartupDiff returns the cached startup consistency diff, or nil if the
// hosts file was consistent on startup.
func (c *Client) GetStartupDiff() (*DiffDTO, error) {
	var out *DiffDTO
	if err := c.call(MethodGetStartupDiff, struct{}{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) CurrentHostsZone() (string, error) {
	var out string
	if err := c.call(MethodCurrentHostsZone, struct{}{}, &out); err != nil {
		return "", err
	}
	return out, nil
}

func (c *Client) CheckPortAvailable(port int) (bool, error) {
	var out bool
	if err := c.call(MethodCheckPortAvailable, CheckPortAvailableParams{Port: port}, &out); err != nil {
		return false, err
	}
	return out, nil
}

func (c *Client) SuggestFreePort(from int) (int, error) {
	var out int
	if err := c.call(MethodSuggestFreePort, SuggestFreePortParams{From: from}, &out); err != nil {
		return 0, err
	}
	return out, nil
}

func (c *Client) IsPrivilegedPort(port int) (bool, error) {
	var out bool
	if err := c.call(MethodIsPrivilegedPort, IsPrivilegedPortParams{Port: port}, &out); err != nil {
		return false, err
	}
	return out, nil
}

// --- Two-phase hosts-write methods ---
// Prepare* asks the host to validate + build the new hosts content (no
// elevation on the host). Complete* tells the host the GUI's elevated write
// succeeded, so the host finalizes (persist config + reconcile forwarders).
// The GUI's App facade orchestrates the elevated write between the two calls.

// PrepareApplyAll validates the full staged domain + forward sets and builds
// the new hosts content on the host (no elevation). The GUI writes it
// elevated only when the result's Changed is true, then calls
// CompleteApplyAll to finalize.
func (c *Client) PrepareApplyAll(domains []DomainDTO, forwards []ForwardDTO) (PrepareApplyAllResult, error) {
	var out PrepareApplyAllResult
	if err := c.call(MethodPrepareApplyAll, SaveAllParams{Domains: domains, Forwards: forwards}, &out); err != nil {
		return PrepareApplyAllResult{OtherError: err.Error()}, err
	}
	return out, nil
}

// CompleteApplyAll tells the host the GUI's elevated write succeeded (or was
// skipped because nothing in the hosts zone changed), so the host should
// reconcile config + forwarders to exactly the given assigned sets.
func (c *Client) CompleteApplyAll(domains []DomainDTO, forwards []ForwardDTO) (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodCompleteApplyAll, SaveAllParams{Domains: domains, Forwards: forwards}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// PreparePause asks the host to build the hosts content with an empty zone
// (no elevation). The GUI writes it elevated only when Changed is true, then
// calls CompletePause.
func (c *Client) PreparePause() (PrepareResult, error) {
	var out PrepareResult
	if err := c.call(MethodPreparePause, struct{}{}, &out); err != nil {
		return PrepareResult{OtherError: err.Error()}, err
	}
	return out, nil
}

// CompletePause finalizes a pause after the GUI's elevated write succeeded
// (or was skipped): sets Paused=true, persists config, stops all forwarders.
func (c *Client) CompletePause() (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodCompletePause, struct{}{}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// PrepareResume asks the host to build the hosts content with one entry per
// Domain (no elevation). The GUI writes it elevated only when Changed is
// true, then calls CompleteResume.
func (c *Client) PrepareResume() (PrepareResult, error) {
	var out PrepareResult
	if err := c.call(MethodPrepareResume, struct{}{}, &out); err != nil {
		return PrepareResult{OtherError: err.Error()}, err
	}
	return out, nil
}

// CompleteResume finalizes a resume after the GUI's elevated write succeeded
// (or was skipped): sets Paused=false, persists config, starts every
// effective forward.
func (c *Client) CompleteResume() (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodCompleteResume, struct{}{}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// PrepareFixNow asks the host to build the hosts content matching the
// effective state (no elevation). The GUI writes it elevated only when
// Changed is true, then calls CompleteFixNow.
func (c *Client) PrepareFixNow() (PrepareResult, error) {
	var out PrepareResult
	if err := c.call(MethodPrepareFixNow, struct{}{}, &out); err != nil {
		return PrepareResult{OtherError: err.Error()}, err
	}
	return out, nil
}

// CompleteFixNow reconciles the forwarder pool to the effective state. Config
// is NOT persisted (FixNow only repairs the hosts file and pool to match the
// existing config).
func (c *Client) CompleteFixNow() (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodCompleteFixNow, struct{}{}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}

// QuitHost tears down the host process (kills any running GUI child and quits
// the systray loop). Called by the GUI's frontend quit-confirmation modal when
// the user confirms quitting while hosting is active. Returns SimpleResult
// {OK:true} on success; the host typically exits shortly after responding, so
// callers should treat a connection-closed error as a successful teardown
// signal too.
func (c *Client) QuitHost() (SimpleResult, error) {
	var out SimpleResult
	if err := c.call(MethodQuitHost, struct{}{}, &out); err != nil {
		return SimpleResult{Error: err.Error()}, err
	}
	return out, nil
}