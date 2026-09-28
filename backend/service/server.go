package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"sync"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/internal/debuglog"
)

// Server speaks the JiejieBox JSON IPC protocol over a pair of streams.
//
// The contract is one complete JSON value per line. Responses are written
// under a mutex because events are emitted from backend goroutines while
// requests are answered on the read loop; interleaving two half-written lines
// would corrupt the stream.
type Server struct {
	backend *Backend
	out     io.Writer

	mu  sync.Mutex
	enc *json.Encoder

	// stopOnce closes stopCh exactly once, letting the read loop finish
	// without waiting for stdin to reach EOF. A normal quit still closes the
	// pipe, but the backend no longer DEPENDS on that: the process can end on
	// its own once teardown has been requested.
	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewServer wires a backend to its output stream.
func NewServer(b *Backend, out io.Writer) *Server {
	enc := json.NewEncoder(out)
	return &Server{backend: b, out: out, enc: enc, stopCh: make(chan struct{})}
}

// requestStop tells the read loop to finish.
//
// Called only AFTER the shutdown ACK has been written, so the client is never
// left waiting for a reply that a stopping loop will not send.
func (s *Server) requestStop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// Stopped reports whether the read loop has been asked to finish.
func (s *Server) Stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// write sends one protocol value as a single line.
func (s *Server) write(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// json.Encoder.Encode appends the newline that delimits the frame.
	if err := s.enc.Encode(v); err != nil {
		debuglog.WarnLog("backend ipc: write failed: %v", err)
	}
}

// SendEvent implements the backend's event sink.
func (s *Server) SendEvent(ev protocol.Event) { s.write(ev) }

// Serve reads requests until the stream closes.
//
// A malformed line is answered with an error and does not kill the loop: a
// single bad frame from a misbehaving client must not take the backend down
// while the user's VPN is running.
func (s *Server) Serve(in io.Reader) {
	unsubscribe := s.backend.Subscribe(s.SendEvent)
	defer unsubscribe()

	sc := bufio.NewScanner(in)
	// Requests can carry config fragments; raise the default 64 KiB limit.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// A blocked Scan cannot be interrupted, so lines are delivered through a
	// channel: the reader goroutine owns stdin, and this loop keeps the ability
	// to finish on the stop signal without waiting for EOF.
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for sc.Scan() {
			// Copy: Scanner reuses its buffer on the next Scan.
			buf := make([]byte, len(sc.Bytes()))
			copy(buf, sc.Bytes())
			select {
			case lines <- buf:
			case <-s.stopCh:
				return
			}
		}
	}()

	for {
		var line []byte
		select {
		case <-s.stopCh:
			if err := sc.Err(); err != nil {
				debuglog.WarnLog("backend ipc: read failed: %v", err)
			}
			return
		case next, ok := <-lines:
			if !ok {
				if err := sc.Err(); err != nil {
					debuglog.WarnLog("backend ipc: read failed: %v", err)
				}
				return
			}
			line = next
		}
		if len(line) == 0 {
			continue
		}

		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			// No id is recoverable, so reply with an empty one; the client
			// logs it and moves on.
			s.write(protocol.Response{Error: &protocol.Error{
				Code:        "bad_request",
				Message:     fmt.Sprintf("cannot decode request: %v", err),
				Recoverable: true,
			}})
			continue
		}
		resp := s.handle(req)
		// A zero ID means the handler already wrote its own response (the
		// shutdown ACK). Every other path sets the ID from the request.
		if resp.ID != "" {
			s.write(resp)
		}
	}
}

// handle dispatches one request. It never panics out: a failure in a handler
// is converted into a structured error so the frontend shows a message
// instead of losing the backend.
// handle resolves one request.
//
// It returns the response for Serve to write, except for the shutdown ACK:
// that one must be flushed before a teardown which may exit the process, so the
// handler writes it itself and returns a zero-ID response as the signal that
// there is nothing left to send.
func (s *Server) handle(req protocol.Request) (resp protocol.Response) {
	defer func() {
		if r := recover(); r != nil {
			debuglog.ErrorLog("backend ipc: panic in %s: %v\n%s", req.Method, r, debug.Stack())
			resp = protocol.Response{ID: req.ID, Error: &protocol.Error{
				Code:        "internal",
				Message:     fmt.Sprintf("backend panic: %v", r),
				Recoverable: false,
			}}
		}
	}()

	switch req.Method {
	case protocol.MethodHandshake:
		return protocol.Response{ID: req.ID, Result: s.backend.Handshake()}

	case protocol.MethodGetAppSnapshot:
		return protocol.Response{ID: req.ID, Result: s.backend.Snapshot()}

	case protocol.MethodSubscribe:
		// The event stream is already wired by Serve; acknowledge so the
		// client knows events will follow.
		return protocol.Response{ID: req.ID, Result: map[string]any{"subscribed": true}}

	case protocol.MethodStartCore:
		if err := s.backend.StartCore(); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.coreState()}

	case protocol.MethodStopCore:
		if err := s.backend.StopCore(); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.coreState()}

	case protocol.MethodRestartCore:
		if err := s.backend.RestartCore(); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.coreState()}

	case protocol.MethodSetCoreMode:
		mode, _ := req.Params["mode"].(string)
		if err := s.backend.SetCoreMode(mode); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.Snapshot()}

	case protocol.MethodSetAutoPing:
		enabled, _ := req.Params["enabled"].(bool)
		if err := s.backend.SetAutoPing(enabled); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.settingsState()}

	case protocol.MethodSetAutoUpdate:
		enabled, _ := req.Params["enabled"].(bool)
		if err := s.backend.SetAutoUpdateSubscriptions(enabled); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: s.backend.settingsState()}

	case protocol.MethodGetProxyGroups:
		list, err := s.backend.ProxyGroups()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: list}

	case protocol.MethodGetProxies:
		group, _ := req.Params["group"].(string)
		list, err := s.backend.Proxies(group)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: list}

	case protocol.MethodSwitchProxy:
		group, _ := req.Params["group"].(string)
		name, _ := req.Params["name"].(string)
		list, err := s.backend.SwitchProxy(group, name)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: list}

	case protocol.MethodTestProxy:
		group, _ := req.Params["group"].(string)
		name, _ := req.Params["name"].(string)
		list, err := s.backend.TestProxy(group, name)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: list}

	case protocol.MethodTestProxyGroup:
		// The response is the FINAL result; progress arrives meanwhile as
		// proxy_test_progress events on the same connection. The request being
		// outstanding must not stop the reader from delivering them — the
		// frontend applies progress and only then sees this return.
		group, _ := req.Params["group"].(string)
		// The request's own lifetime is bounded by the connection, which the
		// server does not model as a context here; the run has its own budget
		// and its own cancellation (supersede / core stop / shutdown), which is
		// what actually needs to interrupt it.
		result, err := s.backend.RunGroupTest(context.Background(), group)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodReloadConfig:
		result, err := s.backend.ReloadConfig()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodAdoptConfig:
		result, err := s.backend.AdoptConfig()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodUpdateSubscriptions:
		result, err := s.backend.UpdateSubscriptions()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodListSubscriptions:
		list, err := s.backend.Subscriptions()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: map[string]any{"subscriptions": list}}

	case protocol.MethodAddSubscription:
		name, _ := req.Params["name"].(string)
		url, _ := req.Params["url"].(string)
		dto, err := s.backend.AddSubscription(name, url)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: dto}

	case protocol.MethodUpdateSubscription:
		id, _ := req.Params["id"].(string)
		name, _ := req.Params["name"].(string)
		url, _ := req.Params["url"].(string)
		// The enabled flag is optional: an edit that does not mention it must
		// not silently clear it, so absence is distinguished from false.
		var enabled *bool
		if v, present := req.Params["enabled"]; present {
			if b, ok := v.(bool); ok {
				enabled = &b
			}
		}
		dto, err := s.backend.UpdateSubscription(id, name, url, enabled)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: dto}

	case protocol.MethodRemoveSubscription:
		id, _ := req.Params["id"].(string)
		if err := s.backend.RemoveSubscription(id); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		list, err := s.backend.Subscriptions()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: map[string]any{"subscriptions": list}}

	case protocol.MethodSetSubscriptionEnabled:
		id, _ := req.Params["id"].(string)
		enabled, _ := req.Params["enabled"].(bool)
		dto, err := s.backend.SetSubscriptionEnabled(id, enabled)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: dto}

	case protocol.MethodRefreshSubscription:
		id, _ := req.Params["id"].(string)
		dto, err := s.backend.RefreshSubscription(id)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: dto}

	case protocol.MethodGetDaemonStatus:
		status, err := s.backend.DaemonStatus()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: status}

	case protocol.MethodDaemonInstall:
		result, err := s.backend.DaemonInstall()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodDaemonStart:
		result, err := s.backend.DaemonStart()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodDaemonRepair:
		result, err := s.backend.DaemonRepair()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodDaemonUninstall:
		purge, _ := req.Params["purge"].(bool)
		result, err := s.backend.DaemonUninstall(purge)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodPairDaemon:
		invite, _ := req.Params["invite"].(string)
		status, err := s.backend.PairDaemon(invite)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: status}

	case protocol.MethodUnpairDaemon:
		status, err := s.backend.UnpairDaemonForget()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: status}

	case protocol.MethodImportCoreFile:
		path, _ := req.Params["path"].(string)
		result, err := s.backend.ImportCoreFile(path)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodImportSubscriptionFile:
		path, _ := req.Params["path"].(string)
		result, err := s.backend.ImportSubscriptionFile(path)
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: result}

	case protocol.MethodSetDaemonKeepRunning:
		keepRunning, _ := req.Params["enabled"].(bool)
		if err := s.backend.SetDaemonKeepRunningAfterQuit(keepRunning); err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		status, err := s.backend.DaemonStatus()
		if err != nil {
			return protocol.Response{ID: req.ID, Error: toProtocolError(err)}
		}
		return protocol.Response{ID: req.ID, Result: status}

	case protocol.MethodShutdown:
		// The ACK must reach the client BEFORE teardown can exit the process.
		//
		// Teardown ends in GracefulExit, which terminates the process. Starting
		// it in a goroutine while the read loop still has to write the response
		// leaves those two racing: if the exit wins, the ACK is never flushed
		// and the client waits for a reply that can no longer arrive.
		//
		// Writing here, on the read-loop goroutine, and only then releasing
		// teardown, makes the order explicit rather than dependent on
		// scheduling.
		s.write(protocol.Response{ID: req.ID, Result: map[string]any{"shutting_down": true}})
		go s.backend.Shutdown()
		// Let the read loop finish on its own. The ACK is already on the wire,
		// so this cannot strand the client, and the backend no longer needs the
		// frontend to close the pipe before it can exit.
		s.requestStop()
		// Zero ID: Serve must not write this one again.
		return protocol.Response{}
	}

	return protocol.Response{ID: req.ID, Error: &protocol.Error{
		Code:        "unknown_method",
		Message:     fmt.Sprintf("unknown method %q", req.Method),
		Recoverable: false,
	}}
}

// toProtocolError converts a handler error into the wire form.
func toProtocolError(err error) *protocol.Error {
	if pe, ok := err.(*protocol.Error); ok {
		return pe
	}
	return &protocol.Error{Code: "internal", Message: err.Error(), Recoverable: true}
}
