package service

import (
	"bytes"
	"runtime"
	"strconv"
)

// goroutineID returns the calling goroutine's id.
//
// GO DOES NOT EXPOSE THIS, and the usual advice is not to want it. This is the exception the
// advice is about: the backend's event dispatcher is a single goroutine, and delivery must ask
// "am I that goroutine?" — a question about the CALLER, which no amount of shared state can
// answer. A shared flag answers "is SOME goroutine delivering?" and is true on every other
// goroutine at the same time, which silently reorders concurrent emitters.
//
// The implementation reads the id from the runtime's own goroutine header. It is deliberately
// the CHEAPEST reliable form: `runtime.Stack` with a full buffer costs microseconds and
// allocates, which is unacceptable on an emit path that runs per traffic sample. This uses a
// small stack buffer, reads only the first line, and parses the number after "goroutine ".
//
// The value is only ever compared for equality against another id from this same function, so
// it is not interpreted as a scheduling identity and cannot go stale in a meaningful way:
// goroutine ids are never reused within a process lifetime.
func goroutineID() uint64 {
	var buf [32]byte
	n := runtime.Stack(buf[:], false)
	// The header is "goroutine 123 [running]:\n".
	line := buf[:n]
	const prefix = "goroutine "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return 0
	}
	line = line[len(prefix):]
	end := bytes.IndexByte(line, ' ')
	if end < 0 {
		return 0
	}
	id, err := strconv.ParseUint(string(line[:end]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
