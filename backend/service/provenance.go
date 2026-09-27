// Config provenance: who owns the config.json on disk.
//
// `reload_config` rebuilds config.json by replaying the wizard state. That is
// only correct for a config the launcher itself produced — replaying state over
// a config someone wrote by hand would destroy their work, and replaying an
// empty state over a working config is worse still.
//
// Existence of state.json cannot answer this. The subscription manager creates
// one the first time a source is added (state.New()), so an externally managed
// config acquires a state file the moment the user adds a subscription — and a
// check based on "does state.json exist" then flips from correctly refusing to
// wrongly rebuilding.
//
// Nor can modification times: a user editing config.json by hand makes it newer
// than the state, and a rebuild makes it newer still, so mtime says nothing
// about ownership.
//
// So ownership is recorded explicitly, in a marker written only when the
// launcher has actually built the config. A marker is the smallest durable fact
// that answers the question, and it stays true across restarts.

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"singbox-launcher/internal/debuglog"
)

// provenanceFileName is the marker's name, kept beside config.json.
//
// It is deliberately not named like a user file and carries no secrets: it
// records only that the launcher built the config next to it, and when.
const provenanceFileName = ".jiejiebox-config.json"

// configProvenance is the marker's contents.
type configProvenance struct {
	// Managed is true when the launcher built the config beside this marker.
	Managed bool `json:"managed"`
	// BuiltAt is when the launcher last wrote the config. Informational.
	BuiltAt string `json:"built_at,omitempty"`
	// AppVersion records which build produced it, for support.
	AppVersion string `json:"app_version,omitempty"`
}

// provenancePath returns the marker path for the active config.
func (b *Backend) provenancePath() string {
	if b.ac == nil || b.ac.FileService == nil {
		return ""
	}
	configPath := b.ac.FileService.ConfigPath
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), provenanceFileName)
}

// configIsRebuildable reports whether a rebuild may safely replace config.json.
//
// Three cases, and the distinction between the last two is the whole point:
//
//  1. No config.json — nothing to overwrite, so a rebuild is how the file comes
//     into existence. Allowed.
//  2. config.json with our marker — the launcher built it, so rebuilding it is
//     exactly what the user is asking for. Allowed.
//  3. config.json with no marker — somebody else owns this file. Refused,
//     whatever else exists in the data directory.
//
// Case 3 is why the state file cannot be used as the signal: the subscription
// manager creates one the first time a source is added, so an external config
// acquires a state file while remaining entirely external.
func (b *Backend) configIsRebuildable() bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}

	// Case 1: nothing to lose.
	if _, err := os.Stat(b.ac.FileService.ConfigPath); os.IsNotExist(err) {
		return true
	}

	// Cases 2 and 3: the marker decides.
	path := b.provenancePath()
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var p configProvenance
	if err := json.Unmarshal(raw, &p); err != nil {
		debuglog.WarnLog("config provenance: unreadable marker %s: %v", path, err)
		return false
	}
	return p.Managed
}

// markConfigManaged records that the launcher now owns the config on disk.
//
// Called only after a build has actually succeeded, so the marker never claims
// ownership of a file the launcher did not write. A failure to record is logged
// but not fatal: the rebuild itself already succeeded, and the consequence is
// only that the next reload will refuse — which is the safe direction.
func (b *Backend) markConfigManaged() error {
	path := b.provenancePath()
	if path == "" {
		return nil
	}
	p := configProvenance{
		Managed:    true,
		BuiltAt:    time.Now().UTC().Format(time.RFC3339),
		AppVersion: b.Handshake().BackendVersion,
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		debuglog.WarnLog("config provenance: cannot write %s: %v", path, err)
		return err
	}
	debuglog.InfoLog("config provenance: marked %s as launcher-managed", filepath.Base(path))
	return nil
}
