// IPC server: listens on a unix socket, reads newline-delimited JSON
// requests, dispatches them to a Handler, and writes newline-delimited JSON
// responses. Used by the host process to expose orchestrator methods to the
// GUI process.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// SocketPath returns the path to the IPC unix socket under ~/.intraflow/.
func SocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".intraflow", "ipc.sock"), nil
}

// Handler dispatches a single RPC method. The handler unmarshals params
// (raw JSON), executes the operation, and returns either a result value (to
// be JSON-encoded into the response) or an error. A handler error is sent
// back as an ErrorBody; the connection stays open for further requests.
type Handler func(method string, params json.RawMessage) (result interface{}, err error)

// Server is the IPC server. Listen opens the socket; Serve accepts
// connections and dispatches requests to the Handler. Close shuts down.
type Server struct {
	handler Handler
	ln      net.Listener
	wg      sync.WaitGroup
	closed  bool
	mu      sync.Mutex

	// conns tracks every currently-connected client connection so
	// BroadcastEvent can fan out a push message to all of them. Guarded by
	// connsMu. Each conn is paired with its own write mutex (created in
	// handleConn) so a slow/dead writer does not block writes to other
	// clients; BroadcastEvent is best-effort and skips a conn on write
	// error (logging it).
	conns   map[net.Conn]*sync.Mutex
	connsMu sync.Mutex
}

// ListenAndServe opens the IPC socket and starts accepting connections. It
// blocks until Close is called. If the socket file already exists (stale from
// a crashed host), it is removed before binding.
func ListenAndServe(h Handler) (*Server, error) {
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ipc: mkdir: %w", err)
	}
	// Remove a stale socket from a previous run.
	_ = unix.Unlink(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}
	// Restrict to the current user.
	_ = os.Chmod(path, 0o600)

	s := &Server{handler: h, ln: ln, conns: make(map[net.Conn]*sync.Mutex)}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			stopped := s.closed
			s.mu.Unlock()
			if stopped {
				return
			}
			log.Printf("ipc: accept: %v", err)
			return
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Register this connection for BroadcastEvent fan-out. We keep a
	// per-conn write mutex (writeMu) here and share the same pointer with
	// the server's conns map so BroadcastEvent serializes against the
	// response writers on each conn without blocking unrelated conns.
	var writeMu sync.Mutex
	s.connsMu.Lock()
	if s.conns == nil {
		s.conns = make(map[net.Conn]*sync.Mutex)
	}
	s.conns[conn] = &writeMu
	s.connsMu.Unlock()
	defer func() {
		s.connsMu.Lock()
		delete(s.conns, conn)
		s.connsMu.Unlock()
	}()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("ipc: read: %v", err)
			}
			return
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(writer, &writeMu, 0, fmt.Sprintf("invalid request: %v", err))
			continue
		}
		result, herr := s.handler(req.Method, req.Params)
		if herr != nil {
			s.writeError(writer, &writeMu, req.ID, herr.Error())
			continue
		}
		s.writeResult(writer, &writeMu, req.ID, result)
	}
}

func (s *Server) writeResult(w *bufio.Writer, mu *sync.Mutex, id int, result interface{}) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeError(w, mu, id, fmt.Sprintf("marshal result: %v", err))
		return
	}
	resp := Response{ID: id, Result: raw}
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	w.Write(out)
	w.WriteByte('\n')
	w.Flush()
}

func (s *Server) writeError(w *bufio.Writer, mu *sync.Mutex, id int, msg string) {
	resp := Response{ID: id, Error: &ErrorBody{Message: msg}}
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	w.Write(out)
	w.WriteByte('\n')
	w.Flush()
}

// BroadcastEvent sends a server→client push event to every currently-connected
// client. It marshals data (which may be nil → omitted) and writes one
// newline-terminated JSON line per client. It is best-effort: a write error on
// one connection is logged and that connection is skipped (it will be
// unregistered when its read loop returns). It is safe to call from any
// goroutine (e.g. an IPC handler or a tray click callback).
func (s *Server) BroadcastEvent(name string, data interface{}) error {
	if name == "" {
		return errors.New("ipc: empty event name")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ipc: marshal event data: %w", err)
	}
	evt := Event{Name: name, Data: raw}
	out, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("ipc: marshal event: %w", err)
	}

	s.connsMu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.connsMu.Unlock()

	for _, c := range conns {
		s.connsMu.Lock()
		mu, ok := s.conns[c]
		s.connsMu.Unlock()
		if !ok {
			// Conn closed between snapshot and write; skip.
			continue
		}
		mu.Lock()
		// Write directly to the conn; we do not share the buffered
		// writer from handleConn (it lives on the read goroutine and is
		// not safe to touch here). A single line write through a fresh
		// buffered writer would flush identically; instead we append
		// '\n' and write the slice in one call to keep the critical
		// section tight.
		_, werr := c.Write(append(out, '\n'))
		mu.Unlock()
		if werr != nil {
			log.Printf("ipc: broadcast %q to %v: %v", name, c.RemoteAddr(), werr)
		}
	}
	return nil
}

// Close stops accepting connections, closes the listener, and waits for
// active connections to finish. It also removes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.ln.Close()
	s.wg.Wait()
	path, perr := SocketPath()
	if perr == nil {
		_ = unix.Unlink(path)
	}
	return err
}

// ErrNotConnected is returned by the client when no connection to the host
// is established.
var ErrNotConnected = errors.New("ipc: not connected to host")
