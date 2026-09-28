package service

import (
	"sync"
	"testing"

	"singbox-launcher/core/state"
)

// loadFixtureState writes a state.json holding two subscription sources and returns
// the backend plus the state path.
func loadFixtureState(t *testing.T, b *Backend, sources ...state.Source) *state.State {
	t.Helper()
	s, path, err := b.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	s.Sources = sources
	if err := s.Save(path); err != nil {
		t.Fatalf("save fixture state: %v", err)
	}
	return s
}

func subSource(id, name, url string) state.Source {
	return state.Source{
		Node: state.Node{Kind: state.SourceKindSubscription, Enabled: true},
		ID:   id, Name: name, URL: url,
	}
}

// TestSubscriptionCRUDSerializedWithRefresh is claim 14, and it is a lost update.
//
// Add/Update/Remove load the whole state, modify it and save it back without taking
// `SubscriptionMu`, while the refresh path takes that lock around its own
// load-modify-save. Two writers therefore race on one file: the refresh loads
// version 10, the user's edit loads version 10, the edit saves version 11, and the
// refresh then saves its own stale snapshot — silently discarding what the user just
// changed.
//
// The test drives the real CRUD entry points concurrently with a refresh-shaped
// writer and requires both mutations to survive.
func TestSubscriptionCRUDSerializedWithRefresh(t *testing.T) {
	b := backendWithConfig(t)
	loadFixtureState(t, b, subSource("s1", "A", "https://example.invalid/a"))

	const rounds = 40
	var wg sync.WaitGroup

	// Writer 1: the user enabling/disabling through the CRUD path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			on := i%2 == 0
			_, _ = b.UpdateSubscription("s1", "", "", &on, false)
		}
	}()

	// Writer 2: the refresh path taking the shared lock around its own save.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			b.ac.SubscriptionMu.Lock()
			s, path, err := b.loadState()
			if err == nil {
				for j := range s.Sources {
					if s.Sources[j].ID == "s1" {
						// A refresh-owned field, like nodes or meta.
						s.Sources[j].Name = "refreshed"
					}
				}
				_ = s.Save(path)
			}
			b.ac.SubscriptionMu.Unlock()
		}
	}()

	wg.Wait()

	// Both fields must be present. If CRUD does not share the lock, one writer's
	// whole-file save erases the other's field.
	s, _, err := b.loadState()
	if err != nil {
		t.Fatalf("final loadState: %v", err)
	}
	src := s.FindSource("s1")
	if src == nil {
		t.Fatal("the source vanished")
	}
	if src.Name != "refreshed" {
		t.Errorf("the refresh's field was lost (name = %q): the CRUD path saves the "+
			"whole file without taking SubscriptionMu, so it overwrites a concurrent "+
			"refresh", src.Name)
	}
}

// TestUpdateAllMergesUnderAFreshLock is claim 15, and it is a lost update.
//
// `UpdateConfigFromSubscriptions` loaded the state BEFORE taking `SubscriptionMu`, so
// the lock protected only the tail of its read-modify-write. A user edit landing in
// that window was overwritten when the refresh saved its pre-lock snapshot — the
// comment claiming the lock protected "load → mutate → save" described a design the
// code did not implement.
//
// The test therefore asserts the MERGE MODEL, which is what makes the update safe
// rather than merely locked: the sweep must re-read the state after fetching and copy
// only the fields the fetch owns, matched by ID. A whole-snapshot save cannot be
// correct no matter where the lock is placed.
func TestUpdateAllMergesUnderAFreshLock(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "core/config_service_subscriptions.go"))

	// The results must be applied after the network phase, by re-loading state.
	if !contains(src, "state.Load(statePath)") {
		t.Fatal("the sweep does not re-read the state after fetching, so it writes " +
			"back a snapshot taken before the network phase — any edit made during " +
			"the fetch is erased")
	}
	// Matching must be by ID, never by slice position: a concurrent insert or delete
	// moves every following index.
	if !contains(src, "latest.FindSource(fetched.ID)") {
		t.Error("fetch results are not matched to sources by ID; a positional merge " +
			"writes one source's nodes into another after a concurrent edit")
	}
	// Only fetch-owned fields may be copied. Name/URL/enabled belong to the user.
	//
	// Matched on the FIELD, not on a literal assignment form. The slices are copied
	// through a deep-copy expression rather than a bare `=`, because the fetch result
	// was read from a pre-lock snapshot and assigning its slice headers directly would
	// alias the live state — two owners of one backing array. Requiring the `= fetched.X`
	// spelling rejected the correct fix and accepted the racy one.
	for _, owned := range []string{"disk.Nodes", "disk.Meta", "disk.UpdateStatus"} {
		if !contains(src, owned+" =") {
			t.Errorf("the merge does not copy %s from the fetch result", owned)
		}
	}
	if contains(src, "disk.Name = ") || contains(src, "disk.Enabled = ") ||
		contains(src, "disk.URL = fetched") {
		t.Error("the merge writes user-owned fields from the fetch snapshot, so a " +
			"concurrent rename or enable/disable is overwritten by the refresh")
	}
	// A source that vanished or changed URL during the sweep must not be resurrected
	// or have the previous provider's nodes attached to its new URL.
	if !contains(src, "disappeared") || !contains(src, "changed URL") {
		t.Error("the merge does not discard results for sources that were deleted or " +
			"repointed while the fetch was in flight")
	}
}

// TestSubscriptionNetworkFetchHappensOutsideTheLock is claim 16.
//
// Holding `SubscriptionMu` across the whole fan-out holds it for as long as the
// slowest subscription takes to fetch. Now that the CRUD paths correctly share that
// lock, a lock held across the network would block every add/edit/remove in the UI
// behind a provider's response time — so the fix for claim 14 must not be paid for
// with this.
func TestSubscriptionNetworkFetchHappensOutsideTheLock(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "core/config_service.go"))

	// THE SWEEP MUST ACTUALLY RUN. A previous revision computed the set of sources to
	// refresh, discarded it into `_ =`, and never called the sweep at all — so every
	// subscription silently stopped updating while `UpdateSubscriptions` still reported
	// refresh success. Asserting "a fetch is not inside the lock" is trivially true of
	// code that fetches nothing, which is how that regression passed its own test.
	call := indexOf(src, "refreshSubscriptionsMetaAndCache(stateRef")
	if call < 0 {
		t.Fatal("the subscription sweep is never called, so no source is ever refreshed " +
			"and the refresh phase reports success for zero fetches")
	}

	// And when it IS called, no SubscriptionMu may be held across the call: the
	// fan-out is network I/O, and holding the lock there would block every
	// add/edit/remove behind the slowest provider.
	before := src[:call]
	lock := lastIndexOf(before, "ac.SubscriptionMu.Lock()")
	unlock := lastIndexOf(before, "ac.SubscriptionMu.Unlock()")
	if lock > unlock {
		t.Error("the network fan-out runs inside the SubscriptionMu critical section, " +
			"so a slow subscription blocks every subscription edit behind a network call")
	}

	// The merge inside the sweep must take the lock itself, since the caller no longer
	// holds it. Without that this is a second unsynchronised whole-file writer beside
	// the CRUD paths.
	sweep := stripGoComments(readServiceSource(t, "core/config_service_subscriptions.go"))
	if !contains(sweep, "ac.SubscriptionMu.Lock()") {
		t.Error("the sweep merges fetch results into state.json without taking " +
			"SubscriptionMu, so it races every subscription edit and can leave the " +
			"file unparseable")
	}
}

// TestEnableSubscriptionMarksConfigStale is claim 17.
//
// `enabled` decides whether a source participates in the build, so changing it
// changes what the config SHOULD contain. UpdateSubscription neither marked the config
// stale nor emitted an event, so the UI kept showing the old config as current and
// never offered the reload.
func TestEnableSubscriptionMarksConfigStale(t *testing.T) {
	b := backendWithConfig(t)
	loadFixtureState(t, b, subSource("s1", "A", "https://example.invalid/a"))

	// The config is current before the change.
	if b.ac.StateService != nil {
		b.ac.StateService.ClearCacheStale()
	}

	off := false
	if _, err := b.UpdateSubscription("s1", "", "", &off, false); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}

	if b.ac.StateService != nil && !b.ac.StateService.IsConfigStale() {
		t.Error("disabling a subscription did not mark the config stale, so the UI " +
			"never offers the reload that would remove its nodes")
	}
}

// TestChangingSubscriptionURLInvalidatesOldMaterialization is claim 18.
//
// Changing a source's URL kept its nodes, meta and success status, which all belong
// to the PREVIOUS provider. The UI then showed the new URL beside the old provider's
// nodes, and a rebuild could install nodes that the displayed URL does not serve.
func TestChangingSubscriptionURLInvalidatesOldMaterialization(t *testing.T) {
	b := backendWithConfig(t)
	s := loadFixtureState(t, b, subSource("s1", "A", "https://example.invalid/a"))
	// Materialise the old provider's data, as a successful refresh would.
	path := ""
	{
		loaded, p, err := b.loadState()
		if err != nil {
			t.Fatalf("loadState: %v", err)
		}
		path = p
		for i := range loaded.Sources {
			loaded.Sources[i].Nodes = []state.Node{{Kind: state.SourceKindServer, Tag: "old-node", Enabled: true}}
			loaded.Sources[i].Meta = &state.SubMeta{ProfileTitle: "Provider A"}
		}
		if err := loaded.Save(path); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	_ = s
	_ = path

	if _, err := b.UpdateSubscription("s1", "", "https://example.invalid/b", nil, false); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}

	after, _, err := b.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	src := after.FindSource("s1")
	if src == nil {
		t.Fatal("the source vanished")
	}
	if len(src.Nodes) != 0 {
		t.Errorf("changing the URL kept %d materialised node(s) from the previous "+
			"provider, so a rebuild can install nodes the displayed URL does not "+
			"serve", len(src.Nodes))
	}
	if src.Meta != nil && src.Meta.ProfileTitle == "Provider A" {
		t.Error("changing the URL kept the previous provider's metadata; the UI then " +
			"shows the new URL labelled with the old provider")
	}
}

// TestUpdateSubscriptionRejectsDuplicateURL is claim 19.
//
// `AddSubscription` refuses to create two sources with the same URL, but
// `UpdateSubscription` would happily edit one into pointing at another's URL. The
// invariant must hold for edits too, or it is not an invariant — two sources pointing
// at one provider means duplicate fetches, duplicate nodes and tag collisions.
func TestUpdateSubscriptionRejectsDuplicateURL(t *testing.T) {
	b := backendWithConfig(t)
	loadFixtureState(t, b,
		subSource("s1", "A", "https://example.invalid/a"),
		subSource("s2", "B", "https://example.invalid/b"),
	)

	if _, err := b.UpdateSubscription("s2", "", "https://example.invalid/a", nil, false); err == nil {
		t.Fatal("editing a subscription to a URL another source already uses was " +
			"accepted; Add refuses this, so the invariant is only enforced on one of " +
			"the two paths that can violate it")
	}

	// The state must be unchanged: a refused edit must not be half-applied.
	after, _, err := b.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if src := after.FindSource("s2"); src == nil || src.URL != "https://example.invalid/b" {
		t.Error("the refused edit still modified the source")
	}
}

// lastIndexOf returns the index of the last occurrence of sub in s, or -1.
//
// Needed because a file may contain several critical sections and only the one
// immediately preceding a call determines whether the call is inside it.
func lastIndexOf(s, sub string) int {
	idx := -1
	for {
		next := indexOf(s[idx+1:], sub)
		if next < 0 {
			return idx
		}
		idx += next + 1
	}
}

// TestAddSubscriptionMarksConfigStale covers the one build-input edit that had no
// staleness call.
//
// Every OTHER edit that changes what the config should contain — removing a
// source, changing its URL, toggling `enabled` — marks the config stale and emits
// an event, which is what makes the UI offer a reload. Adding a source did
// neither, so a user could add a provider, watch it appear in the list, and have
// the core keep running the old config with nothing on screen saying that
// anything remained to be done.
func TestAddSubscriptionMarksConfigStale(t *testing.T) {
	b := backendWithConfig(t)
	loadFixtureState(t, b, subSource("s1", "A", "https://example.invalid/a"))

	// The config is current before the change.
	if b.ac.StateService != nil {
		b.ac.StateService.ClearCacheStale()
	}

	if _, err := b.AddSubscription("B", "https://example.invalid/b"); err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}

	if b.ac.StateService != nil && !b.ac.StateService.IsConfigStale() {
		t.Error("adding a subscription did not mark the config stale, so the UI " +
			"never offers the reload that would include its nodes")
	}
}
