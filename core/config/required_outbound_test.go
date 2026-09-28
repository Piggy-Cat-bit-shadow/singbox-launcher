package config

import (
	"encoding/json"
	"os"
	"testing"

	"singbox-launcher/core/config/configtypes"
)

// loadRealTemplateParserConfig parses bin/wizard_template.json the same way the
// template loader does, so these tests exercise the SHIPPING template rather
// than a hand-written stand-in.
func loadRealTemplateParserConfig(t *testing.T) *ParserConfig {
	t.Helper()
	raw, err := os.ReadFile("../../bin/wizard_template.json")
	if err != nil {
		t.Fatalf("read wizard_template.json: %v", err)
	}
	var tmpl map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	pcRaw, ok := tmpl["parser_config"]
	if !ok {
		t.Fatal("template has no parser_config")
	}
	// The loader wraps the flat parser_config under the "ParserConfig" key
	// (core/template/loader.go). Reproduced here so the fixture matches what the
	// app actually builds from.
	wrapped, _ := json.Marshal(map[string]json.RawMessage{"ParserConfig": pcRaw})
	var pc ParserConfig
	if err := json.Unmarshal(wrapped, &pc); err != nil {
		t.Fatalf("parse parser_config: %v", err)
	}
	if len(pc.ParserConfig.Outbounds) == 0 {
		t.Fatal("template parser_config declares no outbounds; the fixture is wrong")
	}
	return &pc
}

// membersOf reads a selector's member list out of an emitted object.
func membersOf(ob map[string]interface{}) []string {
	if ob == nil {
		return nil
	}
	list, _ := ob["outbounds"].([]interface{})
	out := make([]string, 0, len(list))
	for _, m := range list {
		if s, ok := m.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func nodeFixture(tag string, enabled bool) configtypes.CanonicalNode {
	return configtypes.CanonicalNode{
		Kind: "server", Tag: tag, Enabled: enabled,
		Body: json.RawMessage(`{"type":"shadowsocks","server":"1.2.3.4","server_port":8388,` +
			`"method":"aes-128-gcm","password":"pw"}`),
		OriginRaw: "fixture", OriginKind: "subscription",
	}
}

func withFixtureSource(pc *ParserConfig, nodes ...configtypes.CanonicalNode) {
	pc.ParserConfig.Proxies = append(pc.ParserConfig.Proxies, ProxySource{
		Source:    "https://example.invalid/sub",
		Canonical: &configtypes.CanonicalSource{IsContainer: true, Nodes: nodes},
	})
}

// TestRequiredSelectorSurvivesAnEmptySubscription is the regression for the first
// half of the incident.
//
// With a subscription present but yielding NO enabled nodes, `proxy-out` is
// declared required with `addOutbounds: ["direct-out"]`. It MUST still be
// emitted, holding its own declared fallback. Before the fix it disappeared,
// which is what left `route.final` pointing at a tag that no longer existed.
func TestRequiredSelectorSurvivesAnEmptySubscription(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)
	withFixtureSource(pc, nodeFixture("node-a", false)) // present but DISABLED

	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		t.Fatalf("generation refused a required-selector-only config: %v", err)
	}
	tags := emittedTags(res)
	if !hasTag(tags, "proxy-out") {
		t.Fatalf("required selector %q was not emitted; got tags: %v", "proxy-out", tags)
	}
	ob := emittedObject(t, res, "proxy-out")
	members := membersOf(ob)
	if len(members) == 0 {
		t.Fatal("proxy-out was emitted with NO members, which sing-box rejects outright")
	}
	if !hasTag(members, "direct-out") {
		t.Errorf("proxy-out must hold its declared fallback direct-out, got members %v", members)
	}
}

// TestRequiredSelectorWithNoNodesStillRoutes covers the same declaration but with
// a source that emits nothing at all, which is the fresh-install shape.
func TestRequiredSelectorWithNoNodesStillRoutes(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)
	withFixtureSource(pc) // an enabled source that emits nothing
	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		t.Fatalf("generation refused: %v", err)
	}
	if !hasTag(emittedTags(res), "proxy-out") {
		t.Fatalf("required proxy-out missing; tags=%v", emittedTags(res))
	}
}

// TestNonRequiredEmptySelectorStillDrops pins the OTHER direction: the fix must
// credit the DECLARATION, not switch off the empty-selector rule for everything.
// An empty selector that is not required has no promise to keep, and keeping it
// would emit a group sing-box rejects.
func TestNonRequiredEmptySelectorStillDrops(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)
	found := false
	for i := range pc.ParserConfig.Outbounds {
		if pc.ParserConfig.Outbounds[i].Tag == "vpn ①" {
			pc.ParserConfig.Outbounds[i].Required = false
			pc.ParserConfig.Outbounds[i].AddOutbounds = nil
			found = true
		}
	}
	if !found {
		t.Fatal("template no longer declares 'vpn ①'; this test needs updating, not deleting")
	}
	withFixtureSource(pc, nodeFixture("node-a", false)) // no usable nodes
	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	if hasTag(emittedTags(res), "vpn ①") {
		t.Errorf("an empty NON-required selector must be dropped, but it was emitted: %v",
			emittedTags(res))
	}
}

// TestRequiredCreditIsNotInvented pins that the credit comes from the outbound's
// OWN addOutbounds, never from a substitute the code picks.
//
// A required selector with NO fallback and no nodes cannot be built. The answer
// must not be to invent a member.
func TestRequiredCreditIsNotInvented(t *testing.T) {
	pc := loadRealTemplateParserConfig(t)
	for i := range pc.ParserConfig.Outbounds {
		if pc.ParserConfig.Outbounds[i].Tag == "proxy-out" {
			pc.ParserConfig.Outbounds[i].Required = true
			pc.ParserConfig.Outbounds[i].AddOutbounds = nil // nothing to hold on to
		}
	}
	withFixtureSource(pc, nodeFixture("node-a", false))
	res, err := GenerateOutboundsFromParserConfig(pc, map[string]int{}, nil, DirectionBuildOptions{})
	if err != nil {
		return // explicit refusal is the correct outcome here
	}
	if ob := emittedObject(t, res, "proxy-out"); ob != nil {
		if members := membersOf(ob); len(members) > 0 {
			t.Errorf("proxy-out declares no addOutbounds and has no nodes, yet it was emitted "+
				"with members %v — the fallback was invented rather than declared", members)
		}
	}
}
