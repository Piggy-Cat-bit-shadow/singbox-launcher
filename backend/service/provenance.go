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
// launcher has actually built the config.
//
// WHY THREE STATES AND NOT A BOOL
//
// The first version of this file mapped "no marker" to "external", and that was
// wrong in the most common upgrade path there is: a config.json written by an
// OLDER JiejieBox, before markers existed, has no marker and never did. Calling
// it external told long-standing users that some other tool manages their config
// — a false accusation about their own file, and one that (before the policy
// seam) also blocked the rebuild they were entitled to.
//
// Absence of evidence is not evidence of absence. A missing marker means
// UNKNOWN, and unknown is resolved the only safe way: by checking whether the
// config on disk is what the current state would build anyway (see
// adoptLegacyConfig).

package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muhammadmuzzammil1998/jsonc"

	"crypto/sha256"
	"encoding/hex"
	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
)

// provenanceFileName is the marker's name, kept beside config.json.
//
// It is deliberately not named like a user file and carries no secrets: it
// records only that the launcher built the config next to it, and when.
const provenanceFileName = ".jiejiebox-config.json"

// ConfigOwnership says who is responsible for the config on disk.
//
// The UI must branch on THIS, never on `ConfigRebuildable`: rebuildable is a
// derived permission, and reading "not rebuildable" as "external" is exactly the
// inference that libelled historical JiejieBox configs.
type ConfigOwnership string

const (
	// OwnershipManaged — the launcher wrote this config and may rewrite it.
	OwnershipManaged ConfigOwnership = "managed"
	// OwnershipUnknown — the launcher cannot tell. Either there is no marker at
	// all (an older build's config, or a hand-written one) or the marker is
	// unreadable. The UI explains the ambiguity and offers explicit adoption;
	// nothing is overwritten without the user saying so.
	OwnershipUnknown ConfigOwnership = "unknown"
	// OwnershipExternal — there is positive evidence that another tool owns this
	// file, so the launcher never touches it.
	OwnershipExternal ConfigOwnership = "external"
)

// configProvenance is the marker's contents.
type configProvenance struct {
	// Managed is true when the launcher built the config beside this marker.
	//
	// A pointer so a marker that omits the key is distinguishable from one that
	// sets it false. The distinction matters: an absent key is a marker we do
	// not understand, whereas `managed: false` is a deliberate statement that
	// another tool owns the file.
	Managed *bool `json:"managed,omitempty"`
	// BuiltAt is when the launcher last wrote the config. Informational.
	BuiltAt string `json:"built_at,omitempty"`
	// AppVersion records which build produced it, for support.
	AppVersion string `json:"app_version,omitempty"`

	// ConfigSHA256 binds the marker to the CONTENT it describes.
	//
	// THE POINT OF THE WHOLE FILE. "managed: true" says the launcher built a
	// config here AT SOME TIME. It does not say the file present NOW is that
	// config — and the gap between those two statements is where a user's manual
	// edit gets silently deleted. A user who opens config.json and changes a
	// route, a DNS setting or an outbound leaves `managed: true` untouched, so
	// the next reload or start-rebuild is authorised to overwrite work the
	// launcher never wrote.
	//
	// With the hash, ownership means "the launcher owns what is CURRENTLY here".
	// A mismatch is not proof of a foreign tool: it is proof that the content
	// changed, and the honest response is to ask rather than to assume.
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	// BuildRevision records which generator produced the marked content.
	//
	// Distinct from AppVersion: the app can be reinstalled without the config
	// generator changing, and the generator can change without the app version
	// moving. When a rebuild would produce different bytes for the same state,
	// this is what makes the difference explicable.
	BuildRevision string `json:"build_revision,omitempty"`
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

// configExists reports whether there is a config on disk to reason about.
func (b *Backend) configExists() bool {
	if b.ac == nil || b.ac.FileService == nil || b.ac.FileService.ConfigPath == "" {
		return false
	}
	_, err := os.Stat(b.ac.FileService.ConfigPath)
	return err == nil
}

// configOwnership decides who owns the config, WITHOUT performing adoption.
//
// It is a pure-ish read: no I/O beyond stat and one small file read, no
// network, and no writes. Adoption (which does write) is a separate, explicit
// step so that merely asking the question can never change the answer.
func (b *Backend) configOwnership() ConfigOwnership {
	if b.ac == nil || b.ac.FileService == nil {
		return OwnershipUnknown
	}
	// No config at all: there is nothing to own. Reported as managed because the
	// launcher is free to create one — the same case that used to answer "yes,
	// rebuildable" and still does.
	if !b.configExists() {
		return OwnershipManaged
	}

	path := b.provenancePath()
	if path == "" {
		return OwnershipUnknown
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// THE case this file was rewritten for. A missing marker proves
			// nothing: it is the normal state for any config built before
			// markers existed. Never reported as external.
			return OwnershipUnknown
		}
		debuglog.WarnLog("config provenance: cannot read marker %s: %v", path, err)
		return OwnershipUnknown
	}

	var p configProvenance
	if err := json.Unmarshal(raw, &p); err != nil {
		// A corrupt marker is not a statement about ownership either.
		debuglog.WarnLog("config provenance: malformed marker %s: %v", path, err)
		return OwnershipUnknown
	}
	if p.Managed == nil {
		// Well-formed JSON that says nothing about ownership.
		debuglog.WarnLog("config provenance: marker %s has no 'managed' field", path)
		return OwnershipUnknown
	}
	if *p.Managed {
		// A marker that claims ownership without binding itself to any content
		// cannot answer the question that matters. Either it predates content
		// binding (an older marker format) or it was written by something that
		// did not know what it was writing. Resolving it as MANAGED would grant
		// permission to overwrite a file nobody has verified.
		if p.ConfigSHA256 == "" {
			debuglog.WarnLog("config provenance: marker %s claims ownership without a "+
				"content hash; ownership cannot be verified", path)
			return OwnershipUnknown
		}
		// The marker describes specific content. If the file no longer matches,
		// the launcher does not own what is there now.
		current, err := b.configContentHash()
		if err != nil {
			debuglog.WarnLog("config provenance: cannot hash the config: %v", err)
			return OwnershipUnknown
		}
		if current != p.ConfigSHA256 {
			// Deliberately NOT reported as external. The launcher cannot tell a
			// user's own hand-edit from a foreign tool's rewrite, and accusing the
			// user of being someone else is the same mistake this file already
			// made once with historical configs. UNKNOWN is the honest answer: the
			// UI explains and offers explicit adoption.
			debuglog.InfoLog("config provenance: the config changed since the launcher " +
				"wrote it; ownership is no longer verifiable")
			return OwnershipUnknown
		}
		return OwnershipManaged
	}
	// An explicit `managed: false` is positive evidence of another owner. This
	// is the ONLY path that yields external.
	return OwnershipExternal
}

// configIsRebuildable reports whether a rebuild may safely replace config.json.
//
// Derived from ownership, and deliberately narrower than "managed": a config
// that does not exist yet is rebuildable because building it is how it comes
// into existence, while UNKNOWN and EXTERNAL are refused. The refusal is a
// permission, not a claim — the UI must not read it as "external" (see
// ConfigOwnership).
func (b *Backend) configIsRebuildable() bool {
	return b.configOwnership() == OwnershipManaged
}

// markConfigManaged records that the launcher now owns the config on disk.
//
// Called only after a build has actually succeeded, so the marker never claims
// ownership of a file the launcher did not write. A failure to record is logged
// but not fatal: the rebuild itself already succeeded, and the consequence is
// only that the next reload will refuse — which is the safe direction.
func (b *Backend) markConfigManaged() error {
	return b.markConfigManagedBytes(nil)
}

// markConfigManagedBytes records ownership, describing `promoted` when the caller knows
// the exact bytes it installed.
//
// WHY THE BYTES ARE PASSED IN RATHER THAN RE-READ. This used to hash the file, with a
// comment saying that made the marker describe "the winner's bytes" if another writer
// raced. That reasoning is backwards for the one caller that has the authoritative
// answer: the promotion hook is HANDED the bytes it promoted, and re-reading the file
// afterwards deliberately discards that knowledge. Between the rename and the read,
// anything else on the machine can replace the file — an editor, a sync tool, the
// daemon — and the marker then claims `managed: true` for content the launcher never
// produced, which is precisely the "hand-edited config treated as ours" defect this
// marker exists to prevent, reopened through a window the caller did not need to open.
//
// `promoted == nil` still hashes the file. That is the honest answer for callers that
// did not perform the promotion themselves (adoption, explicit re-marking): they know
// only what is there NOW. Unknown provenance must degrade to a fresh read, not to an
// assumption.
func (b *Backend) markConfigManagedBytes(promoted []byte) error {
	path := b.provenancePath()
	if path == "" {
		return nil
	}
	hash := ""
	if promoted != nil {
		sum := sha256.Sum256(promoted)
		hash = hex.EncodeToString(sum[:])
	} else {
		var err error
		hash, err = b.configContentHash()
		if err != nil {
			debuglog.WarnLog("config provenance: cannot hash the config to mark it: %v", err)
			return err
		}
	}
	managed := true
	p := configProvenance{
		Managed:       &managed,
		BuiltAt:       time.Now().UTC().Format(time.RFC3339),
		AppVersion:    b.Handshake().BackendVersion,
		ConfigSHA256:  hash,
		BuildRevision: b.buildRevisionForProvenance(),
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// Atomic, for the same reason the config itself is: a marker truncated by an
	// interruption is malformed JSON, which reads as UNKNOWN and leaves the user
	// explaining a state the launcher inflicted on itself.
	if err := writeFileAtomic(path, append(raw, '\n'), 0o644); err != nil {
		debuglog.WarnLog("config provenance: cannot write %s: %v", path, err)
		return err
	}
	debuglog.InfoLog("config provenance: marked %s as launcher-managed (sha256 %.12s)",
		filepath.Base(path), hash)
	return nil
}

// configContentHash is the SHA-256 of the config currently on disk.
//
// The digest itself comes from `hashConfigBytes`, which is also what records the identity of
// the config a RUNNING core loaded. Two implementations of "hash this config" would
// eventually disagree, and the comparison between "what is on disk" and "what the core
// loaded" would then report divergence that does not exist — or miss the divergence that
// does.
func (b *Backend) configContentHash() (string, error) {
	if b.ac == nil || b.ac.FileService == nil || b.ac.FileService.ConfigPath == "" {
		return "", fmt.Errorf("no config path")
	}
	raw, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		return "", err
	}
	return hashConfigBytes(raw), nil
}

// buildRevisionForProvenance reports the generator revision recorded alongside the
// marked content. Empty when unavailable: the field is informational, and a missing
// revision must not make the marker unusable.
func (b *Backend) buildRevisionForProvenance() string {
	if rev, ok := core.ReadBuildRevisionForService(b.ac.FileService.ConfigPath); ok {
		return rev
	}
	return ""
}

// writeFileAtomic replaces path with content, via a temp file in the same directory
// and a rename.
//
// The file is fsynced before the rename and the directory after it, so a crash
// leaves either the old file or the new one — never a truncated one. Same directory
// because rename is only atomic within a filesystem.
func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// No-op once the rename has succeeded.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Best-effort: the rename is already durable in the metadata as far as the app
	// is concerned, and a failure to fsync the directory must not fail the write.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// canonicalJSON re-encodes JSON so two documents can be compared structurally.
//
// Ownership cannot be decided by bytes: the same config round-tripped through a
// different encoder differs in key order, spacing and indentation while being
// the same configuration. Numbers are compared as json.Number so that 5000 and
// 5e3 — which decode to the same float but are different text — are still
// distinguished where it matters, and no precision is lost to float64.
//
// Comments are stripped FIRST, through the same jsonc helper the rest of the
// project uses. This is not a nicety: sing-box configs are JSONC, and every
// config this launcher has ever written carries the template's `//` comments.
// Feeding one straight to encoding/json fails with "invalid character '/'", so
// without this step the comparison could never succeed on a real config and
// adoption would silently never happen — the feature would look implemented
// while doing nothing at all.
func canonicalJSON(raw []byte) ([]byte, error) {
	raw = jsonc.ToJSON(raw)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Re-marshalling a map sorts keys, and encoding/json emits a stable form for
	// the same structure, so equal structures produce equal bytes.
	return json.Marshal(v)
}

// configMatchesState reports whether the config on disk is structurally equal to
// the one the CURRENT state would build.
//
// This is the evidence that lets a legacy config be adopted. It is deliberately
// a structural, not byte, comparison, because a config written by an older build
// differs in formatting while representing the same configuration.
//
// Every value participates, secrets included: comparing only a subset could
// declare two different configs equal, which would hand ownership of a file the
// launcher did not write. A build failure, or any unreadable input, is reported
// as "cannot prove" (false) rather than as a match.
func (b *Backend) configMatchesState(candidate []byte) bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}
	onDisk, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		debuglog.InfoLog("config provenance: cannot read config for adoption check: %v", err)
		return false
	}
	want, err := canonicalJSON(candidate)
	if err != nil {
		debuglog.InfoLog("config provenance: candidate config is not valid JSON: %v", err)
		return false
	}
	got, err := canonicalJSON(onDisk)
	if err != nil {
		// A config the launcher cannot parse is certainly not one it can claim
		// to have produced.
		debuglog.InfoLog("config provenance: on-disk config is not valid JSON: %v", err)
		return false
	}
	return bytes.Equal(want, got)
}

// adoptLegacyConfig attempts a ONE-TIME, evidence-based adoption of a config
// that has no provenance marker.
//
// The upgrade path this exists for: a config.json written by an older JijieBox,
// before markers existed. It has no marker and never did, so it reads as UNKNOWN
// and — without this — would stay unmanaged forever, leaving a long-standing user
// unable to reload their own config and (until the policy seam) warned that some
// other tool owned it.
//
// Adoption is granted only when the launcher can PROVE the file is its own work:
// the config on disk must be structurally identical to the config the current
// state would build. That is a real proof of authorship in the only sense that
// matters — the launcher's own state reproduces this exact file — and it is why
// nothing here trusts the file's LOCATION. A hand-written config parked in the
// data directory does not match the state and is therefore never adopted, which
// is the trap that "it lives in DataDir, so it is ours" would have walked into.
//
// Strictly read-only with respect to the user's data: it does not write
// config.json, does not touch state.json, and does not fetch anything. The build
// reuses the same pipeline a rebuild would, but takes only its in-memory bytes.
// A build that fails, or any input it cannot read, means "cannot prove", which
// means UNKNOWN — never adoption.
//
// Attempted at most ONCE per backend instance. A failed attempt usually means
// the config genuinely is not ours — a hand-written one, or one with no state to
// reproduce it — and that verdict will not change while the process runs, so
// retrying on every snapshot would spend a full config build (measured: ~8 ms)
// per snapshot, on the UI's critical path, forever. The cost is paid once, and a
// config that later becomes adoptable is picked up on the next launch.
//
// Returns true only if the marker was written, so a caller can report the new
// ownership immediately.
func (b *Backend) adoptLegacyConfig() bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}
	b.adoptOnce.once.Do(func() { b.adoptOnce.succeeded = b.tryAdoptLegacyConfig() })
	return b.adoptOnce.succeeded
}

// tryAdoptLegacyConfig performs the single adoption attempt.
func (b *Backend) tryAdoptLegacyConfig() bool {
	// Only ever for a config that exists and has no verdict yet. An explicit
	// `managed: false` is evidence of another owner and must not be reconsidered.
	if b.configOwnership() != OwnershipUnknown || !b.configExists() {
		return false
	}

	candidate, err := b.buildCandidateConfig()
	if err != nil {
		// Expected in several legitimate situations (no state, unreadable
		// template, no materialized nodes). Not an error the user needs: the
		// outcome is simply that ownership stays UNKNOWN.
		debuglog.InfoLog("config provenance: cannot build a candidate for adoption (%v) — ownership stays unknown", err)
		return false
	}
	if !b.configMatchesState(candidate) {
		debuglog.InfoLog("config provenance: on-disk config does not match what the current state builds — " +
			"not adopting (ownership stays unknown)")
		return false
	}
	if err := b.markConfigManaged(); err != nil {
		return false
	}
	debuglog.InfoLog("config provenance: adopted a legacy config — it reproduces from the current state")
	return true
}

// buildCandidateConfig builds the config the current state would produce,
// WITHOUT writing it anywhere.
//
// Delegates to the same core pipeline a rebuild uses, so "what the launcher
// would write" has one definition and cannot drift from what a rebuild actually
// writes. The guards around it are the point: no forced rebuild, no template
// download, no state mutation.
func (b *Backend) buildCandidateConfig() ([]byte, error) {
	if b.ac == nil {
		return nil, fmt.Errorf("no app controller")
	}
	return b.ac.BuildConfigReadOnly()
}

// installConfigPromotionProvenance makes every config promotion record provenance.
//
// Called once when the backend is built. This is what closes the transaction hole:
// rather than asking each rebuild caller to remember to mark ownership, the mark
// happens at the one place a config can reach disk. A path that promotes a config
// and forgets is no longer expressible.
//
// The marker is written for the bytes ACTUALLY promoted, so it can never describe
// content other than what the build produced.
func installConfigPromotionProvenance(b *Backend) {
	if b == nil {
		return
	}
	core.SetConfigPromotionHook(func(configPath string, promoted []byte) {
		if b.ac == nil || b.ac.FileService == nil {
			return
		}
		// Only for OUR config: a build into a check-only location must not
		// claim ownership of a file the launcher did not install.
		if configPath != b.ac.FileService.ConfigPath {
			return
		}
		// The promoted bytes, not a fresh read: this callback is the one place that
		// knows exactly what was installed.
		if err := b.markConfigManagedBytes(promoted); err != nil {
			// Best-effort, exactly as the build-revision marker is: the build
			// already succeeded, and failing it now would discard a valid config
			// over a bookkeeping write. A missing marker degrades ownership to
			// UNKNOWN, which is the safe direction.
			debuglog.WarnLog("config provenance: the config was promoted but ownership "+
				"was not recorded: %v", err)
		}
	})
}
