package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"singbox-launcher/core"
)

// TestManagedMarkerInvalidatedByExternalConfigEdit is claim 11, and it is the
// difference between "the launcher has managed this file" and "the launcher owns
// what is in this file NOW".
//
// The marker recorded only `managed: true`. A user who then opened config.json and
// changed a route, a DNS setting or an outbound left the marker untouched, so the
// launcher still considered the file its own — and the next reload, start rebuild or
// forced restart silently deleted the user's work.
//
// A marker must therefore bind to the CONTENT it describes.
func TestManagedMarkerInvalidatedByExternalConfigEdit(t *testing.T) {
	b := backendWithConfig(t)
	configPath := b.ac.FileService.ConfigPath

	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	// The launcher builds the config and records ownership of THAT content.
	if err := b.markConfigManaged(); err != nil {
		t.Fatalf("markConfigManaged: %v", err)
	}
	if got := b.configOwnership(); got != OwnershipManaged {
		t.Fatalf("ownership right after a build = %q, want managed", got)
	}

	// The user edits the file by hand. The change is a real config edit, not a
	// reformat: an extra field.
	edited := strings.Replace(string(original), "\"route\":",
		"\"experimental\": {\"clash_api\": {\"external_controller\": \"127.0.0.1:9999\"}},\n  \"route\":", 1)
	if edited == string(original) {
		t.Fatal("fixture did not change; the test would pass vacuously")
	}
	if err := os.WriteFile(configPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("write edited config: %v", err)
	}

	// The marked content is no longer what is on disk, so the launcher may NOT
	// treat it as its own and overwrite it.
	if got := b.configOwnership(); got == OwnershipManaged {
		t.Fatal("a hand-edited config is still reported as launcher-managed; the " +
			"next rebuild will silently delete the user's edit")
	}
	if b.configIsRebuildable() {
		t.Error("a hand-edited config is still reported as rebuildable")
	}
}

// TestManagedMarkerSurvivesAReformatOnlyEdit — the mirror case, so the check is
// about CONTENT and not about mtime or byte identity.
//
// The marker must not become so brittle that ordinary rewriting invalidates it.
// Content is compared by hash, so a file the launcher wrote is recognised again
// after being rewritten with identical bytes.
func TestManagedMarkerSurvivesAReformatOnlyEdit(t *testing.T) {
	b := backendWithConfig(t)
	configPath := b.ac.FileService.ConfigPath

	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := b.markConfigManaged(); err != nil {
		t.Fatalf("markConfigManaged: %v", err)
	}

	// Rewrite the SAME bytes (a touch, an atomic replace of identical content).
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := b.configOwnership(); got != OwnershipManaged {
		t.Fatalf("ownership after rewriting identical content = %q, want managed; the "+
			"marker is keyed on something other than content", got)
	}
}

// TestProvenanceMarkerIsWrittenAtomically is claim 13.
//
// The marker was written with a bare os.WriteFile. An interruption mid-write leaves
// malformed JSON, which reads as UNKNOWN — fail-closed, but a self-inflicted state
// the launcher then has to explain to the user. The config itself is already written
// through a temp file and a rename; the marker must be too.
func TestProvenanceMarkerIsWrittenAtomically(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/provenance.go"))

	idx := indexOf(src, "func (b *Backend) markConfigManaged()")
	if idx < 0 {
		t.Fatal("markConfigManaged not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	_ = src[idx : idx+end]

	// The writer itself must go through the atomic helper, and that helper must
	// use a rename rather than overwriting in place.
	//
	// Scanned from the file, not from markConfigManaged's body: the body moved into
	// markConfigManagedBytes when the marker started describing the bytes it was
	// handed rather than re-reading the file. Anchoring on the function that no longer
	// performs the write made this assertion silently vacuous — the fixed comment
	// stripper is what exposed it.
	if !contains(src, "writeFileAtomic(") {
		t.Error("the provenance marker is not written through the atomic helper, so an " +
			"interrupted write leaves malformed JSON and the ownership answer " +
			"degrades to unknown")
	}
	helperIdx := indexOf(src, "func writeFileAtomic(")
	if helperIdx < 0 {
		t.Fatal("writeFileAtomic not found")
	}
	helperEnd := indexOf(src[helperIdx:], "\nfunc ")
	if helperEnd < 0 {
		helperEnd = len(src) - helperIdx
	}
	helper := src[helperIdx : helperIdx+helperEnd]
	if !contains(helper, "os.CreateTemp(") || !contains(helper, "os.Rename(") {
		t.Error("the atomic writer does not use a temp file plus a rename")
	}
	if !contains(helper, "Sync()") {
		t.Error("the atomic writer does not fsync, so the content is not durable " +
			"before the rename")
	}
}

// TestProvenanceRecordsTheHashItDescribes — the marker must actually contain the
// binding, not merely be validated by one computed elsewhere.
func TestProvenanceRecordsTheHashItDescribes(t *testing.T) {
	b := backendWithConfig(t)
	if err := b.markConfigManaged(); err != nil {
		t.Fatalf("markConfigManaged: %v", err)
	}

	raw, err := os.ReadFile(b.provenancePath())
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var p configProvenance
	if err := jsonUnmarshalForTest(raw, &p); err != nil {
		t.Fatalf("marker is not JSON: %v", err)
	}
	if p.ConfigSHA256 == "" {
		t.Fatal("the marker records no content hash, so it cannot say WHICH config it " +
			"describes — a user edit leaves it looking valid")
	}
	// The revision comes from the build-revision marker the core writes at
	// promotion. Without one there is no revision to record, and an empty string is
	// the honest answer — but the FIELD must exist, or a known revision could never
	// be carried.
	marker := stripGoComments(readServiceSource(t, "backend/service/provenance.go"))
	if !contains(marker, "BuildRevision") {
		t.Error("the marker has no build-revision field, so a config produced by a " +
			"different generator version cannot be recognised")
	}
}

// TestEveryConfigPromotionRecordsProvenance is claim 12, and it is the transaction
// hole.
//
// Only the maintenance reload and the explicit adoption recorded ownership. The
// paths that actually build the config — a classic start's pre-start rebuild, a
// daemon start, a restart's forced rebuild, the auto-rebuild after a subscription
// update, and the first start on a fresh install — promoted config.json and recorded
// nothing.
//
// The fresh-install case is the sharpest: no config and no marker reads as "managed,
// go ahead"; the build creates config.json and still no marker; and the NEXT
// ownership question sees a config with no marker and answers UNKNOWN. The launcher
// disowns the file it just created.
//
// The fix is not to add a call at each site — that is how the gap appeared, and every
// future writer would have to remember again. The invariant is enforced at the one
// place a config can reach disk: the promotion itself.
func TestEveryConfigPromotionRecordsProvenance(t *testing.T) {
	core := stripGoComments(readServiceSource(t, "core/rebuild_corereject.go"))

	if !contains(core, "noteConfigPromoted(") {
		t.Fatal("the config promotion point does not announce the promotion, so a " +
			"config can reach disk without anything recording that the launcher " +
			"built it")
	}

	// The announcement must happen after the config is actually in place, and in
	// the same transaction as the promotion.
	promote := indexOf(core, "if err := promoteCandidate(candidate, l.configPath); err != nil {")
	note := indexOf(core, "noteConfigPromoted(l.configPath, round.ConfigJSON)")
	if promote < 0 || note < 0 {
		t.Fatal("expected both a promotion and a provenance announcement")
	}
	if note < promote {
		t.Error("provenance is announced BEFORE the config is promoted; an " +
			"interruption between them would leave the launcher claiming a file it " +
			"never wrote")
	}

	// And the service layer must actually install the hook, or the announcement
	// reaches nobody.
	svc := stripGoComments(readServiceSource(t, "backend/service/provenance.go"))
	if !contains(svc, "SetConfigPromotionHook(") {
		t.Error("the service layer never installs a promotion hook, so the " +
			"announcement is a no-op and no config records provenance")
	}
	be := stripGoComments(readServiceSource(t, "backend/service/backend.go"))
	if !contains(be, "installConfigPromotionProvenance(") {
		t.Error("the hook is never installed at backend construction")
	}
}

// TestFreshInstallConfigIsRecognisedAsOurs is the fresh-install case of claim 12, and
// the one a user meets on their very first run.
//
// After the launcher builds a config where none existed, the NEXT ownership question
// must answer MANAGED. Before the fix it answered UNKNOWN, because nothing had
// recorded the file the launcher had just written.
func TestFreshInstallConfigIsRecognisedAsOurs(t *testing.T) {
	b := backendWithConfig(t)
	configPath := b.ac.FileService.ConfigPath

	// Simulate the first build: remove both the config and any marker, so the state
	// is exactly a fresh install.
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	_ = os.Remove(b.provenancePath())

	// With no config at all, building one is permitted.
	if got := b.configOwnership(); got != OwnershipManaged {
		t.Fatalf("ownership with no config = %q, want managed (there is nothing to "+
			"protect and building is how it comes into existence)", got)
	}

	// The build writes the config. The service layer's hook records provenance for
	// the promoted bytes.
	body := []byte(`{"outbounds":[],"route":{}}`)
	if err := os.WriteFile(configPath, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	installConfigPromotionProvenance(b)
	core.SetConfigPromotionHook(func(p string, promoted []byte) {
		if p == configPath {
			_ = b.markConfigManaged()
		}
	})
	core.NoteConfigPromotedForTest(configPath, body)

	// The launcher must recognise the file it just created.
	if got := b.configOwnership(); got != OwnershipManaged {
		t.Fatalf("the launcher does not recognise the config it just built "+
			"(ownership = %q); on a fresh install it disowns its own file and "+
			"refuses to manage it", got)
	}
}

// jsonUnmarshalForTest decodes JSON without importing encoding/json at call sites.
func jsonUnmarshalForTest(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
