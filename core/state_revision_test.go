package core

import (
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/core/state"
)

// writeTestState persists a state with one subscription source holding one node.
func writeTestState(t *testing.T, path string, url string, nodeTag string, _ string) {
	t.Helper()
	s := &state.State{}
	src := state.Source{
		Node: state.Node{Kind: state.SourceKindSubscription, Enabled: true},
		ID:   "src-1",
		Name: "Provider",
		URL:  url,
	}
	src.Nodes = []state.Node{{Kind: state.SourceKindServer, Tag: nodeTag, Enabled: true}}
	s.Sources = []state.Source{src}
	if err := s.Save(path); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

// TestCoreRejectCommitMergesIntoLatestState is statement 5 (§34 name).
//
// The disabler holds the state it loaded when the BUILD began, and `Commit` saved that
// whole snapshot back. A build is not instantaneous — it renders, runs `sing-box check`,
// and may loop several rounds while the core names rejected nodes — so anything the user
// changed in that window (a subscription URL, an enable/disable, a new source, DNS, rules,
// vars) was silently reverted by the commit that was only supposed to record which nodes
// the core refused.
//
// This is the stale-snapshot full-file write, in the one place with the longest window.
func TestCoreRejectCommitMergesIntoLatestState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	writeTestState(t, path, "https://old.example/sub", "node-a", "1.1.1.1")

	// The build loads state here.
	loaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := &savedStateDisabler{s: loaded, path: path}

	// The core names a node, so the disabler marks it...
	found := false
	for i := range loaded.Sources {
		if loaded.Sources[i].Kind == state.SourceKindSubscription {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture has no subscription source")
	}
	link := state.NodeLink{FolderID: "src-1", Tag: "node-a"}
	if !d.Disable(link, "unsupported field") {
		t.Fatal("the disabler refused to disable the node")
	}

	// ...and MEANWHILE the user edits unrelated things.
	writeTestState(t, path, "https://NEW.example/sub", "node-a", "")
	{
		edited, lerr := state.Load(path)
		if lerr != nil {
			t.Fatalf("reload for edit: %v", lerr)
		}
		edited.Sources[0].Name = "renamed by the user"
		if serr := edited.Save(path); serr != nil {
			t.Fatalf("save edit: %v", serr)
		}
	}

	if err := d.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := state.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(after.Sources) == 0 {
		t.Fatal("the commit erased every source")
	}
	if got := after.Sources[0].URL; got != "https://NEW.example/sub" {
		t.Errorf("the URL the user changed during the build was reverted to %q; the "+
			"core-reject commit must merge into the LATEST state, not save the "+
			"snapshot it loaded when the build began", got)
	}
	if got := after.Sources[0].Name; got != "renamed by the user" {
		t.Errorf("an unrelated user edit made during the build was lost (name=%q)", got)
	}
	// And the node's rejection must still have been recorded.
	if after.Sources[0].Nodes[0].CoreRejectedReason() == "" {
		t.Error("the rejection the commit existed to record was not recorded")
	}
}

// TestCoreRejectCommitPreservesConcurrentNodeAdditions — the same defect for nodes.
func TestCoreRejectCommitPreservesConcurrentNodeAdditions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeTestState(t, path, "https://a.example/sub", "node-a", "")

	loaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := &savedStateDisabler{s: loaded, path: path}
	if !d.Disable(state.NodeLink{FolderID: "src-1", Tag: "node-a"}, "rejected") {
		t.Fatal("could not disable")
	}

	// The user adds a second node while the build runs.
	fresh, err := state.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	fresh.Sources[0].Nodes = append(fresh.Sources[0].Nodes,
		state.Node{Kind: state.SourceKindServer, Tag: "node-b", Enabled: true})
	if err := fresh.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := d.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := state.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	tags := map[string]bool{}
	for _, n := range after.Sources[0].Nodes {
		tags[n.Tag] = true
	}
	if !tags["node-b"] {
		t.Error("a node added while the build ran was erased by the core-reject commit")
	}
	if !tags["node-a"] {
		t.Error("the rejected node disappeared")
	}
}

// TestOldBuildCannotClearStaleAfterNewerStateSave is statement 4 (§34 name).
//
// A build records nothing about WHICH state it rendered. A slow build (template fetch,
// render, `sing-box check`) that started at revision 100 and commits after the user
// saved revision 101 therefore promotes a config derived from 100 and then clears the
// stale marker — so the launcher reports "config is current" while it is one revision
// behind. Invariant D: a build made from an older state revision may never mark a newer
// revision fresh.
func TestOldBuildCannotClearStaleAfterNewerStateSave(t *testing.T) {
	src := stripCommentsForTest(readCoreSource(t, "core/rebuild.go"))

	if !strings.Contains(src, "stateRevision") && !strings.Contains(src, "StateRevision") &&
		!strings.Contains(src, "revision") {
		t.Fatal("a build records no revision of the state it rendered, so a build that " +
			"started before a user edit can commit afterwards and clear the stale " +
			"marker for a state it never saw")
	}
}

// TestRebuildRecordsTheRevisionItRendered — the mechanism, checked where it must live:
// the build must capture the revision BEFORE it renders, and compare it at commit.
func TestRebuildRecordsTheRevisionItRendered(t *testing.T) {
	src := stripCommentsForTest(readCoreSource(t, "core/rebuild.go"))

	if !strings.Contains(src, "ConfigRevision") && !strings.Contains(src, "revision") {
		t.Fatal("no revision is captured at build start")
	}

	// The capture must come BEFORE the rendering work, and the comparison AFTER it. The
	// distinguishing evidence is relative order, not proximity: the capture is assigned
	// from the loaded state, and the build (the snapshot construction) must follow it.
	load := strings.Index(src, "s, err := state.Load(statePath)")
	capture := strings.Index(src, "stateRevision := s.Revision()")
	build := strings.Index(src, "buildSnapshotFromState(s, layout")
	if load < 0 || capture < 0 || build < 0 {
		t.Fatalf("expected a state load, a revision capture and a build (load=%d "+
			"capture=%d build=%d)", load, capture, build)
	}
	if !(load < capture && capture < build) {
		t.Errorf("the revision must be captured after the load and before the build "+
			"it describes (load=%d capture=%d build=%d)", load, capture, build)
	}
	// And the commit-time comparison must exist.
	if !strings.Contains(src, "revisionMovedSinceBuild(") {
		t.Error("no commit-time comparison, so an old build can still clear the stale " +
			"marker for a state it never rendered")
	}
}

// TestStateCarriesARevisionThatChangesOnSave — the foundation the two tests above need.
//
// Without a revision that increments on every save, "is this build current?" cannot be
// answered at all, and the staleness marker has to guess.
func TestStateCarriesARevisionThatChangesOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeTestState(t, path, "https://a.example/sub", "node-a", "")

	first, err := state.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r1 := first.Revision()
	if r1 == 0 {
		t.Fatal("a loaded state reports revision 0; a build cannot record which " +
			"revision it rendered without a revision")
	}

	first.Sources[0].Name = "renamed"
	if err := first.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	second, err := state.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.Revision() == r1 {
		t.Fatalf("the revision did not change across a save (still %d); a build that "+
			"started before this save cannot detect that its config is now old", r1)
	}
}
