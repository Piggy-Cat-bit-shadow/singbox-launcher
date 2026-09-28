package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"

	"singbox-launcher/internal/debuglog"
)

// runtimeConfig tracks WHICH config content the running core actually loaded.
//
// The identity is stored as BYTES, not only as a hash, because the comparison has to be
// content-aware rather than byte-exact: a config re-serialised with different indentation
// is the SAME config to the core, and reporting it as a divergence would tell the user to
// restart for a change that does not exist. Holding the bytes costs a few kilobytes and
// removes any need to reconstruct them later.
//
// The bug this exists to prevent is a silent divergence: the Clash API answers questions
// about the config the RUNNING core was started with, while every other part of the app —
// the group picker, the proxy list, the switch command — reads config.json from disk. When
// those differ the UI confidently reports and switches groups that the live core does not
// have, and the operation fails with an error that names a group the user can see on
// screen. Nothing in the app compared the two, so the divergence could persist
// indefinitely.
//
// The identity is captured at the moment the core reports itself running, which is the
// only point where "what is loaded" is knowable.
type runtimeConfig struct {
	mu sync.RWMutex
	// sha is the content hash of the config the running core was started with; empty
	// when nothing is running or the identity could not be read.
	sha string
	// path is the file that was read, so a path change alone is detected.
	path string
	// body is the config content itself, for the canonical comparison.
	body []byte
}

// set records the identity of the config that just went live.
func (r *runtimeConfig) set(path, sha string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = path
	r.sha = sha
	// Copied, because the caller may reuse or mutate its buffer.
	r.body = append([]byte(nil), body...)
}

// clear forgets the identity, because no core is running.
func (r *runtimeConfig) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = ""
	r.sha = ""
	r.body = nil
}

// snapshot returns the recorded identity.
func (r *runtimeConfig) snapshot() (path, sha string, body []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.path, r.sha, r.body
}

// recordRunningConfig captures the identity of the config the core just loaded.
//
// Called on the running transition. A failure is logged and recorded as empty rather than
// propagated: this exists to make a divergence VISIBLE, and refusing to start the core
// because a hash could not be computed would turn a diagnostic into an outage.
func (b *Backend) recordRunningConfig() {
	if b.ac == nil || b.ac.FileService == nil {
		return
	}
	path := b.ac.FileService.ConfigPath
	sha, err := b.configContentHash()
	if err != nil {
		debuglog.WarnLog("runtime config: the core is running but its config identity "+
			"could not be read (%v); config/state consistency cannot be checked", err)
		b.runtimeCfg.clear()
		return
	}
	// The bytes are read once, here, because this is the moment they are known to be what
	// the core loaded. Reading them again later would return whatever is on disk THEN,
	// which is precisely the thing being compared against.
	body, err := os.ReadFile(path)
	if err != nil {
		debuglog.WarnLog("runtime config: cannot read %s to record what the core "+
			"loaded: %v", path, err)
		b.runtimeCfg.clear()
		return
	}
	b.runtimeCfg.set(path, sha, body)
	debuglog.InfoLog("runtime config: core is running with %s (sha256 %.12s…)",
		path, sha)
}

// ConfigMatchesRuntime reports whether config.json on disk is the content the running core
// loaded, and whether a core is running at all.
//
// `running == false` means nothing is live, so there is nothing to diverge from and callers
// should fall back to reading the file — which is correct, because a stopped core will load
// whatever is on disk when it next starts.
func (b *Backend) ConfigMatchesRuntime() (matches bool, running bool) {
	path, sha, body := b.runtimeCfg.snapshot()
	if sha == "" {
		return false, false
	}
	if b.ac == nil || b.ac.FileService == nil {
		return false, false
	}
	// A different file is a divergence even if the bytes happen to match.
	if b.ac.FileService.ConfigPath != path {
		return false, true
	}
	current, err := os.ReadFile(path)
	if err != nil {
		// The file is unreadable, which is a divergence in the direction that matters:
		// the core is running something this process can no longer confirm.
		return false, true
	}
	return sameConfigContent(body, current), true
}

// RuntimeConfigDiverged reports whether the running core is serving a config that is no
// longer what is on disk, i.e. a rebuild happened after the core started.
//
// This is the condition behind "the group picker offers a group the core does not have":
// the rebuild added or renamed selector groups, and the running core still has the old set.
// The remedy is a core restart, which the caller decides — not this function.
func (b *Backend) RuntimeConfigDiverged() bool {
	matches, running := b.ConfigMatchesRuntime()
	return running && !matches
}

// hashConfigBytes is the single content hash used for config identity comparisons.
//
// One function so the identity recorded at start and the identity checked later cannot
// drift apart: two implementations of "hash this config" would eventually disagree about
// whitespace or key order, and the comparison would report divergence that is not there.
func hashConfigBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sameConfigContent compares two config snapshots for identity.
func sameConfigContent(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if bytes.Equal(a, b) {
		return true
	}
	// Fall back to the canonical form, so a reformatted but equivalent config is not
	// reported as a change. A config that was re-serialised with different indentation is
	// the SAME config to the core.
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}
