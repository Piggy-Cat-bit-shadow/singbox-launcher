package service

import (
	"bufio"
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

	mu   sync.Mutex
	enc  *json.Encoder
	stop func()
}

// NewServer wires a backend to its output stream.
func NewServer(b *Backend, out io.Writer) *Server {
	enc := json.NewEncoder(out)
	return &Server{backend: b, out: out, enc: enc}
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

	for sc.Scan() {
		line := sc.Bytes()
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
		s.write(resp)
	}
	if err := sc.Err(); err != nil {
		debuglog.WarnLog("backend ipc: read failed: %v", err)
	}
}

// handle dispatches one request. It never panics out: a failure in a handler
// is converted into a structured error so the frontend shows a message
// instead of losing the backend.
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

	case protocol.MethodShutdown:
		// Acknowledge before tearing down so the client is not left waiting.
		go s.backend.Shutdown()
		return protocol.Response{ID: req.ID, Result: map[string]any{"shutting_down": true}}
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
