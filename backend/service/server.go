package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/internal/debuglog"
)

// ipcDrainTimeout bounds how long Serve waits for in-flight replies when it is
// asked to finish. Short: the request that triggers a finish is the one that most
// needs the loop to stop promptly.
const ipcDrainTimeout = 3 * time.Second

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

	// writeFailed closes when the output stream is broken. A client that can no
	// longer be written to is a client that can no longer be served, and the
	// backend must not keep running as if someone were listening.
	writeOnce   sync.Once
	writeFailed chan struct{}
	// connCtx is cancelled when the connection fails, so in-flight work is
	// abandoned instead of continuing to compute replies nobody will read.
	connCtx    context.Context
	connCancel context.CancelFunc

	// inflight tracks running requests so shutdown can wait for the ones that
	// are still producing replies, without blocking the read loop meanwhile.
	inflight sync.WaitGroup
}

// NewServer wires a backend to its output stream.
func NewServer(b *Backend, out io.Writer) *Server {
	enc := json.NewEncoder(out)
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		backend: b, out: out, enc: enc,
		stopCh:      make(chan struct{}),
		writeFailed: make(chan struct{}),
		connCtx:     ctx, connCancel: cancel,
	}
}

// ConnContext is cancelled when this connection can no longer serve its client.
//
// Long handlers take it so that a client that has gone away does not leave the
// backend computing a reply for nobody.
func (s *Server) ConnContext() context.Context { return s.connCtx }

// failConnection records that the client is unreachable.
//
// Called from the writer, where a failed encode is the only evidence that the
// pipe is gone. It cancels in-flight work and asks the read loop to finish.
func (s *Server) failConnection(reason string) {
	s.writeOnce.Do(func() {
		debuglog.WarnLog("backend ipc: connection failed (%s); tearing down", reason)
		s.connCancel()
		close(s.writeFailed)
		s.requestStop()
	})
}

// requestStop tells the read loop to finish.
//
// Called only AFTER the shutdown ACK has been written, so the client is never
// left waiting for a reply that a stopping loop will not send.
func (s *Server) requestStop() {
	s.stopOnce.Do(func() {
		// CANCEL IN-FLIGHT WORK, THEN ASK THE LOOP TO FINISH.
		//
		// Cancelling here, not only in `failConnection`, is what makes `drain`'s wait an
		// orderly one. On a normal shutdown the connection is still healthy, so nothing
		// had cancelled the handlers: a request blocked in a network sweep would run to
		// completion while `drain` waited out its timeout and then gave up, logging that
		// "a handler is still running and its reply will be lost" and leaving the goroutine
		// alive against a connection the caller had already closed. The client's own
		// shutdown timer then fired while the helper was still running, and the next launch
		// found two processes.
		//
		// The context is per-connection and this is the last thing the connection will do,
		// so cancelling it here cannot affect a later request.
		s.connCancel()
		close(s.stopCh)
	})
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
//
// EVERY WRITE IS TRACKED BY THE IN-FLIGHT COUNT, including ones made outside a request
// handler. Events are written by subscriber goroutines that no request owns, so a
// `drain()` that counted only handlers still let the stream be written after `Serve`
// returned — a caller that then read it raced a live writer, which is what the race
// detector caught. The count makes "Serve has returned" mean "nothing is writing any
// more", which is the property callers actually need.
func (s *Server) write(v any) {
	s.inflight.Add(1)
	defer s.inflight.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	// json.Encoder.Encode appends the newline that delimits the frame.
	if err := s.enc.Encode(v); err != nil {
		// A write failure is not a log line: it is the discovery that the client
		// is gone. Previously the backend kept its subscriber attached,
		// generating traffic and lifecycle events into a pipe nobody read, and a
		// Classic core kept running with no frontend to stop it. The connection
		// is now treated as failed, which cancels in-flight work and asks the
		// read loop to finish so the normal EOF teardown runs.
		s.mu.Unlock()
		s.failConnection(err.Error())
		s.mu.Lock()
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
	// Unsubscribe BEFORE draining, not after. `defer` runs last, which left a window
	// where the loop had already decided to finish but was still subscribed: a backend
	// event emitted in that window started a write to a stream the caller was about to
	// consider closed. Detaching first means the drain below waits for writes that were
	// already running, and no new ones can begin.
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
		case <-s.writeFailed:
			// The client is unreachable. Stop reading: nothing we parse can be
			// answered, and finishing here lets the caller's teardown run.
			debuglog.WarnLog("backend ipc: stopping the read loop after a write failure")
			unsubscribe()
			s.drain()
			return
		case <-s.stopCh:
			if err := sc.Err(); err != nil {
				debuglog.WarnLog("backend ipc: read failed: %v", err)
			}
			unsubscribe()
			s.drain()
			return
		case next, ok := <-lines:
			if !ok {
				if err := sc.Err(); err != nil {
					debuglog.WarnLog("backend ipc: read failed: %v", err)
				}
				unsubscribe()
				s.drain()
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
		// DISPATCH MUST NOT BLOCK THE READ LOOP.
		//
		// This call used to run inline, which serialised every IPC method: while
		// `start_core` waited on a config build, a password prompt or a daemon
		// apply, the server did not read the next line at all. A `stop_core` the
		// user then sent sat unread in the pipe, so "stop cancels start" was not
		// a lifecycle bug to fix — the command never reached the controller. The
		// same wait blocked shutdown, mode switches and group tests.
		//
		// The loop now hands each request to its own goroutine and returns to
		// reading immediately. Ordering is preserved where it matters and
		// nowhere else: methods that mutate one domain serialise inside that
		// domain (the lifecycle controller for core operations, the apply mutex
		// for the daemon, the subscription lock for state.json), and a request
		// that wants to interrupt another is exactly the request that must NOT
		// queue behind it. Responses carry their request id, so out-of-order
		// replies are already expressible by the protocol.
		s.inflight.Add(1)
		go func(req protocol.Request) {
			defer s.inflight.Done()
			// THE REQUEST RUNS UNDER THE CONNECTION'S CONTEXT.
			//
			// `connCtx` was stored, documented as the thing that abandons in-flight work,
			// and read by nobody — so a request that outlived its client could not be told
			// about it. `drain()` could only wait out its timeout, log "a handler is still
			// running and its reply will be lost", and leave the goroutine computing a
			// reply for a pipe that is gone; the client's shutdown timer then fires while
			// the helper is still alive, and the next launch finds two processes.
			//
			// Passing it in makes cancellation a mechanism instead of a comment.
			ctx, cancel := context.WithCancel(s.connCtx)
			defer cancel()
			resp := s.handleCtx(ctx, req)
			// A zero ID means the handler already wrote its own response (the
			// shutdown ACK). Every other path sets the ID from the request.
			if resp.ID != "" {
				s.write(resp)
			}
		}(req)
	}
}

// drain waits for requests that were already dispatched to finish writing.
//
// Returning from Serve with handlers still running would let a caller observe an
// incomplete response stream — the shutdown path in particular writes its ACK from
// a handler, and a Serve that returned first would race that write. Waiting here
// keeps the previous guarantee ("when Serve returns, every accepted request has
// been answered") while still letting the loop dispatch concurrently, which is the
// property the read loop needed.
//
// Requests that have NOT yet been read are simply never dispatched: the loop stops
// reading the moment it is asked to finish, so a client cannot have a reply
// outstanding for a line the server never took.
// drain waits for in-flight handlers, with a hard cap.
//
// The WAIT IS BOUNDED ON EVERY EXIT PATH, including the write-failure and stop
// branches: a wedged handler holding a network sweep must not be able to park the read
// loop past its deadline, or the client's own shutdown timer fires while a helper is
// still running and the next launch finds two of them.
//
// The watcher goroutine is not leaked when the cap fires. `WaitGroup.Wait` cannot be
// cancelled, so it stays blocked until the handler finishes — which may be never. It is
// therefore detached deliberately and documented rather than left as an invisible leak:
// it exits as soon as the wedged handler does, and it holds only a channel.
func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(ipcDrainTimeout):
		// A handler is wedged. Losing the reply is better than hanging the exit,
		// which is the whole reason the loop is allowed to finish early.
		debuglog.WarnLog("backend ipc: giving up on in-flight replies after %s; a handler "+
			"is still running and its reply will be lost", ipcDrainTimeout)
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
// handle executes one request under the connection's context.
//
// The entry point the tests and the dispatcher share: a request always runs under the
// connection it arrived on, so there is no way to invoke a handler outside that lifetime.
func (s *Server) handle(req protocol.Request) (resp protocol.Response) {
	ctx, cancel := context.WithCancel(s.connCtx)
	defer cancel()
	return s.handleCtx(ctx, req)
}

// handleCtx executes one request under an explicit context.
//
// The context is the connection's, so a client that has gone away — or a shutdown that has
// begun — cancels the work instead of leaving it to produce a reply nobody will read.
func (s *Server) handleCtx(ctx context.Context, req protocol.Request) (resp protocol.Response) {
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
		// The run is bounded by the CONNECTION as well as by its own budget and its
		// subsystem cancellation (supersede / core stop / shutdown).
		//
		// This passed context.Background() with a comment saying the server modelled
		// no connection context — true when it was written, and no longer true. The
		// gap it left: a frontend that went away left the group test running to its
		// full network budget with nobody to receive the progress or the result, and
		// the backend holding a worker pool against a client that no longer exists.
		result, err := s.backend.RunGroupTest(ctx, group)
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
		// Clearing a custom name is an explicit request, not an empty string.
		//
		// The same absence-vs-empty distinction as `enabled`, and for the same
		// reason: `name: ""` also means "this edit does not mention the name", so
		// without a separate flag there was NO way to express "remove my custom
		// name and go back to the provider's title". The frontend had no such
		// action and the backend had no such input, so the operation the user
		// wanted did not exist anywhere in the product.
		clearName := false
		if v, present := req.Params["clear_name"]; present {
			if b, ok := v.(bool); ok {
				clearName = b
			}
		}
		dto, err := s.backend.UpdateSubscription(id, name, url, enabled, clearName)
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
