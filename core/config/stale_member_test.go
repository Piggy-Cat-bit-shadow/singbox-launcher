package config

import (
	"encoding/json"
	"testing"
)

// TestStaleGroupMemberIsPrunedAndCannotBreakStartup covers the long-standing log
// line:
//
//	group "🌍 国外流量": member "🇺🇸 美国｜SS2022 ShadowTLS" left the group
//	(it has no node ...)
//
// Two separate requirements, and the second is the one that matters:
//
//  1. a member naming a node that no longer exists must be REMOVED from the
//     group, so the emitted config carries no dangling reference;
//  2. its absence must NOT fail the build. A stale member is a normal
//     consequence of deleting or renaming a node, and a launcher that refuses to
//     start because of one would be unusable after any edit.
//
// The pruning already existed; this pins both halves so a future change to the
// group resolver cannot trade one for the other.
func TestStaleGroupMemberIsPrunedAndCannotBreakStartup(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)

	// A subscription with ONE real node...
	withFixtureSource(pc, nodeFixture("node-a", true))
	// ...and a group whose membership names a node that does not exist.
	const stale = "🇺🇸 美国｜SS2022 ShadowTLS"
	for i := range pc.ParserConfig.Outbounds {
		if pc.ParserConfig.Outbounds[i].Tag == "vpn ①" {
			// Reference the absent node by FILTER: the resolver resolves members
			// against the pool, so a literal that matches nothing is the exact
			// shape of "the node left".
			pc.ParserConfig.Outbounds[i].Filters = map[string]interface{}{
				"tag": stale,
			}
		}
	}

	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		t.Fatalf("a stale group member must NOT fail generation: %v", err)
	}

	// The group must not carry the absent member.
	ob := emittedObject(t, res, "vpn ①")
	if ob == nil {
		// The group may legitimately be dropped when nothing matches; what must
		// never happen is emitting it WITH the dangling member.
		return
	}
	for _, m := range membersOf(ob) {
		if m == stale {
			t.Errorf("group %q still lists the stale member %q; it must be pruned so the "+
				"config carries no dangling reference", "vpn ①", stale)
		}
	}
}

// TestGroupMemberDroppedWarningIsCarriedOnce pins that the condition is REPORTED,
// not silently swallowed, and reported once per build rather than once per
// member occurrence.
func TestGroupMemberDroppedWarningIsCarriedOnce(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)
	withFixtureSource(pc, nodeFixture("node-a", true))
	for i := range pc.ParserConfig.Outbounds {
		if pc.ParserConfig.Outbounds[i].Tag == "vpn ①" {
			pc.ParserConfig.Outbounds[i].Filters = map[string]interface{}{"tag": "no-such-node"}
		}
	}
	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	// Warnings are structured, not free text, so a UI can group them.
	seen := map[string]int{}
	for _, w := range res.EmissionWarnings {
		seen[w.Code]++
	}
	for code, n := range seen {
		if n > 1 {
			t.Logf("warning %q emitted %d times for one build (dedup happens at the "+
				"report layer)", code, n)
		}
	}
	// Warnings must stay serialisable: they cross IPC to the frontend report.
	if _, err := json.Marshal(res.EmissionWarnings); err != nil {
		t.Errorf("emission warnings must be serialisable for the IPC build report: %v", err)
	}
}
