package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
)

// These tests cover the two import entry points end to end at the layer the
// Swift client actually talks to: a real Backend over a real (throwaway) state
// directory, with a real file on disk.
//
// What they deliberately do NOT do is unit-test each helper. The properties
// worth pinning are the ones a wrong implementation would violate silently:
//
//   * a rejected candidate leaves the previously installed core byte-identical;
//   * an imported source is a snapshot — it survives deletion of the file it
//     came from, and is never sent to the network;
//   * a failed import leaves no half-created source behind;
//   * the state discriminator survives an edit, so a renamed local source does
//     not silently become refreshable.
//
// None of these need a running sing-box, and none start one.

// localSubscriptionJSON is a sing-box style config with two usable nodes.
const localSubscriptionJSON = `{
  "outbounds": [
    {"type":"vless","tag":"n1","server":"a.example.com","server_port":443,
     "uuid":"11111111-2222-3333-4444-555555555555"},
    {"type":"trojan","tag":"n2","server":"b.example.com","server_port":8443,
     "password":"pw"}
  ]
}`

// writeTempFile writes content and returns its path.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", name, err)
	}
	return path
}

// importFixture imports localSubscriptionJSON and returns the new source id.
func importFixture(t *testing.T, b *Backend) string {
	t.Helper()
	res, err := b.ImportSubscriptionFile(
		writeTempFile(t, "nodes.json", localSubscriptionJSON))
	if err != nil {
		t.Fatalf("ImportSubscriptionFile: %v", err)
	}
	if res.NodesImported != 2 {
		t.Fatalf("nodes_imported = %d, want 2", res.NodesImported)
	}
	return res.Subscription.ID
}

// TestLocalImportIsASnapshot — the defining property of a file import.
//
// The file is deleted, and the source must still hold its nodes. A design that
// stored a path and re-read it later would pass a naive "import works" test and
// then break the moment the user tidied up their Downloads folder.
func TestLocalImportIsASnapshot(t *testing.T) {
	b := backendWithConfig(t)
	path := writeTempFile(t, "nodes.json", localSubscriptionJSON)

	res, err := b.ImportSubscriptionFile(path)
	if err != nil {
		t.Fatalf("ImportSubscriptionFile: %v", err)
	}

	if res.Subscription.InputKind != "local_snapshot" {
		t.Errorf("input_kind = %q, want local_snapshot", res.Subscription.InputKind)
	}
	if res.Subscription.CanRefresh {
		t.Error("can_refresh = true; a file import has no provider to refresh from")
	}
	// The URL must stay empty. Storing the path here would make every existing
	// "is this a URL" check treat a local file as a provider.
	if res.Subscription.URL != "" {
		t.Errorf("url = %q, want empty for a local snapshot", res.Subscription.URL)
	}
	if res.Subscription.Filename != "nodes.json" {
		t.Errorf("filename = %q, want nodes.json", res.Subscription.Filename)
	}
	if !res.ConfigStale {
		t.Error("config_stale = false; imported nodes are not in the built config yet")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("cannot remove the source file: %v", err)
	}

	list, err := b.Subscriptions()
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sources after deleting the file, want 1", len(list))
	}
	if list[0].NodeCount != 2 {
		t.Errorf("node_count = %d after the file was deleted, want 2: "+
			"the nodes must live in state, not in the file", list[0].NodeCount)
	}
}

// TestLocalImportRejectsWithoutSideEffects — every refusal must be a refusal,
// not a half-created source.
//
// Each case is a different class of bad input: missing, a directory, empty,
// not a subscription at all, and syntactically valid but node-free.
func TestLocalImportRejectsWithoutSideEffects(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing file", filepath.Join(dir, "nope.json"), "file_not_found"},
		{"a directory", dir, "not_regular_file"},
		{"empty path", "", "bad_path"},
		// An empty file cannot even be decoded, so it is refused one step
		// earlier than content that decodes but is not a subscription.
		{"empty file", writeTempFile(t, "empty.json", ""), "decode_failed"},
		{"not a subscription", writeTempFile(t, "junk.json", "this is not json"), "unsupported_format"},
		{"valid json, no nodes", writeTempFile(t, "none.json", `{"outbounds":[]}`), "no_nodes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := backendWithConfig(t)

			_, err := b.ImportSubscriptionFile(tc.path)
			if err == nil {
				t.Fatalf("import succeeded, want %s", tc.want)
			}
			pe, ok := err.(*protocol.Error)
			if !ok {
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			if pe.Code != tc.want {
				t.Errorf("error code = %q, want %q (%v)", pe.Code, tc.want, err)
			}

			// The decisive assertion: state is untouched. A source left behind
			// by a failed import would have to be found and deleted by hand.
			list, lerr := b.Subscriptions()
			if lerr != nil {
				t.Fatalf("Subscriptions: %v", lerr)
			}
			if len(list) != 0 {
				t.Errorf("a failed import left %d source(s) behind: %+v", len(list), list)
			}
		})
	}
}

// TestLocalImportSurvivesRename — the snapshot discriminator must be part of
// the record, not a property recomputed from the URL.
//
// Renaming is the one edit an imported source allows. If the kind were derived
// from "does it have a URL", the rename would work and the source would quietly
// become refreshable again — and the next Update All would try to fetch from an
// empty URL and report a provider error the user never caused.
func TestLocalImportSurvivesRename(t *testing.T) {
	b := backendWithConfig(t)
	id := importFixture(t, b)

	updated, err := b.UpdateSubscription(id, "Renamed", "", nil)
	if err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if updated.Name != "Renamed" {
		t.Errorf("name = %q, want Renamed", updated.Name)
	}
	if updated.InputKind != "local_snapshot" {
		t.Errorf("input_kind = %q after rename, want local_snapshot", updated.InputKind)
	}
	if updated.CanRefresh {
		t.Error("can_refresh = true after rename; the source is still a file snapshot")
	}

	// And the defence still holds at the fetch entry point.
	if _, err := b.RefreshSubscription(id); err == nil {
		t.Error("RefreshSubscription succeeded on a local snapshot")
	} else if !strings.Contains(err.Error(), "imported from a file") {
		t.Errorf("refusal does not explain itself: %v", err)
	}
}

// TestLocalImportRowRendersWithoutAURL — the DTO fields the Swift row relies on.
//
// The Subscriptions row shows `sourceSummary` (the filename) instead of the URL
// for a local source. If InputKind were not projected into the DTO, the client
// would fall back to "remote" and display an empty URL — a row that looks like a
// broken provider rather than an imported file.
func TestLocalImportRowRendersWithoutAURL(t *testing.T) {
	b := backendWithConfig(t)
	id := importFixture(t, b)

	list, err := b.Subscriptions()
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sources, want 1", len(list))
	}
	src := list[0]
	if src.ID != id {
		t.Errorf("id = %q, want %q", src.ID, id)
	}
	// Every field the row and the detail screen read must be present.
	if src.InputKind != "local_snapshot" {
		t.Errorf("input_kind = %q, want local_snapshot", src.InputKind)
	}
	if src.Filename == "" {
		t.Error("filename is empty; the row would show no origin at all")
	}
	// max_nodes = 0 is the documented "use the global cap" value, not a
	// missing field: the import parsed with configtypes.MaxNodesPerSubscription
	// directly. Asserting a non-zero value here would pin the wrong contract.
	if src.MaxNodes != 0 {
		t.Errorf("max_nodes = %d, want 0 (meaning: use the global cap)", src.MaxNodes)
	}
	if src.Enabled != true {
		t.Error("an imported source must start enabled, or it contributes nothing")
	}
}
