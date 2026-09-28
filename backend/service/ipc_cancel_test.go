package service

import (
	"strings"
	"testing"
)

// TestRequestContextReachesHandlers is statement 19 (§34 name).
//
// `ConnContext()` exists, is documented as "cancelled when the connection fails, so
// in-flight work is [abandoned]", and is called by NOTHING. Handlers dispatch with no
// context at all, so the cancellation the method describes is not a mechanism the code has —
// it is a method nobody uses.
//
// The visible consequence is in `drain()`: when a handler is wedged in a network sweep the
// server cannot cancel it, so it waits out `ipcDrainTimeout`, gives up, logs that "a handler
// is still running and its reply will be lost", and leaves the goroutine running against a
// connection that no longer exists. The client's own shutdown timer then fires while a
// helper is still alive, and the next launch finds two.
//
// Cancellation is the difference between abandoning a handler and asking it to stop.
func TestRequestContextReachesHandlers(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/server.go"))

	// The dispatch must hand the request a context derived from the connection's, so that
	// cancelling the connection reaches the handler.
	dispatch := functionBodyForTest(t, src, "func (s *Server) Serve(")
	if !strings.Contains(dispatch, "s.connCtx") && !strings.Contains(dispatch, "ConnContext()") &&
		!strings.Contains(dispatch, "s.requestContext(") {
		t.Error("the dispatch starts handlers without a context derived from the " +
			"connection's, so nothing can cancel a request that is already running: " +
			"drain() can only abandon it, and the goroutine outlives the connection")
	}

	// And the context must actually be accepted by the handler path, not just derived.
	handleCtx := functionBodyForTest(t, src, "func (s *Server) handleCtx(")
	if !strings.Contains(handleCtx, "context.Context") {
		t.Error("the handler takes no context, so there is nothing for a cancellation to reach")
	}
	// The plain entry point must derive from the connection, so a handler cannot be invoked
	// outside the connection's lifetime.
	handle := functionBodyForTest(t, src, "func (s *Server) handle(")
	if !strings.Contains(handle, "s.connCtx") {
		t.Error("handle() does not derive from the connection's context, so a request can " +
			"outlive the client that sent it")
	}

	if strings.Count(src, "s.connCtx") < 2 {
		t.Error("connCtx is read in fewer than two places, so it is not the shared " +
			"cancellation source ConnContext() documents")
	}
}

// TestCancellationReachesALongRunningRequest — the behavioural half.
//
// A request that is running when the connection goes away must observe the cancellation
// rather than running to completion. Both teardown paths must cancel, not just the
// failure path: on a NORMAL shutdown the connection is still healthy, so if only
// `failConnection` cancelled, a handler blocked in a network sweep would keep running while
// `drain` waited out its timeout and gave up.
func TestCancellationReachesALongRunningRequest(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/server.go"))

	if !strings.Contains(src, "connCancel") {
		t.Fatal("no cancel function is stored for the connection")
	}

	// `requestStop` is the normal shutdown path; `failConnection` is the broken-pipe path.
	// Both must cancel, and a stored-and-never-called cancel is the same defect as a
	// verifiedAt that is written and never read.
	stop := functionBodyForTest(t, src, "func (s *Server) requestStop(")
	if !strings.Contains(stop, "connCancel()") {
		t.Error("the normal stop path does not cancel in-flight requests. The connection is " +
			"still healthy there, so nothing else would: drain() can only abandon the " +
			"handler, and the goroutine outlives the connection it was serving")
	}
	fail := functionBodyForTest(t, src, "func (s *Server) failConnection(")
	if !strings.Contains(fail, "connCancel()") {
		t.Error("the failure path does not cancel in-flight requests")
	}
}
