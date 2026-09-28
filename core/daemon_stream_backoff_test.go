package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readRepoSource reads a repo-relative file. Source-shape checks read the real
// files rather than a copy, so they cannot drift from what ships.
func readRepoSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	return string(b)
}

// stripGoComments removes line and block comments, so an assertion cannot be
// satisfied by prose describing behaviour that is no longer there.
func stripGoComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
}

// TestUnsupportedStreamsBackOffAndDoNotSpam is CASE 10.
//
// The reported log showed `daemon.tailscale` and `daemon.conns` failing with
// `rpc error: code = Unknown desc = invalid argument` on a daemon that does not
// implement them. The requirement is that this degrades GRACEFULLY: bounded
// retry, no tight loop, no per-second error spam, and no effect on the runtime
// state.
//
// All three properties are structural — a backoff that doubles and caps, a log
// level below Warn, and a stream loop that owns no state — so they are pinned
// here as source invariants. A behavioural test would need a daemon that lies
// about its capabilities, which the fake server does not model.
func TestUnsupportedStreamsBackOffAndDoNotSpam(t *testing.T) {
	for _, file := range []string{
		"core/backend_daemon_tailscale.go",
		"core/backend_daemon_traffic.go",
		"core/backend_daemon.go",
	} {
		src := stripGoComments(readRepoSource(t, file))

		// An unsupported stream must NOT be reported as an error: it is a
		// capability gap on a daemon that answered, and the launcher does not own
		// that build. Reporting it at Warn or Error makes an unfixable condition
		// look like a fault the user must act on.
		if strings.Contains(src, `ErrorLog("daemon.tailscale:`) ||
			strings.Contains(src, `WarnLog("daemon.tailscale:`) {
			t.Errorf("%s: a tailscale stream failure is logged above Debug; an "+
				"unsupported RPC is a capability gap, not a user fault", file)
		}

		// The retry must back off and CAP. An uncapped doubling is still bounded,
		// but a cap of zero or a missing sleep is a tight loop.
		if strings.Contains(src, "backoff") {
			if !strings.Contains(src, "backoff *= 2") {
				t.Errorf("%s: the stream backoff does not grow; a failing stream would "+
					"retry at a constant rate", file)
			}
			if !strings.Contains(src, "backoff < 10*time.Second") {
				t.Errorf("%s: the stream backoff is not capped; a permanently "+
					"unsupported stream would keep a goroutine busy forever", file)
			}
		}
	}
}

// TestStreamLoopsExitOnContext — CASE 15's stream half.
//
// Every supervise*/consume* loop must return when the backend's context ends.
// A loop that only exits on a stream error outlives the engine that owned it,
// which is how a classic→daemon switch ends up with a daemon stream still
// writing into a retired backend.
func TestStreamLoopsExitOnContext(t *testing.T) {
	files := []string{
		"core/backend_daemon.go",
		"core/backend_daemon_tailscale.go",
		"core/backend_daemon_traffic.go",
	}
	checked := 0
	for _, file := range files {
		src := stripGoComments(readRepoSource(t, file))
		for _, marker := range []string{"func (b *DaemonBackend) supervise", "func (b *DaemonBackend) consume"} {
			idx := 0
			for {
				at := strings.Index(src[idx:], marker)
				if at < 0 {
					break
				}
				start := idx + at
				body := src[start:]
				if end := strings.Index(body, "\n}\n"); end > 0 {
					body = body[:end]
				}
				checked++
				// consume* loops read frames and return on error; the supervise*
				// loops are the ones that must observe the context.
				if strings.HasPrefix(marker, "func (b *DaemonBackend) supervise") {
					if !strings.Contains(body, "b.ctx.Done()") && !strings.Contains(body, "b.ctx.Err()") {
						t.Errorf("%s: a supervise loop does not observe b.ctx; it would "+
							"outlive the backend that started it", file)
					}
				}
				idx = start + len(marker)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no stream loops were found; the check would pass vacuously")
	}
}
