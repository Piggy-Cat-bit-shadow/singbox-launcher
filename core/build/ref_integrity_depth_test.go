package build

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests for the SECOND pass over reference integrity.
//
// The first pass caught a dangling `route.final` and a dangling top-level rule
// target. These tests cover the ways the same defect class survived that pass:
// references one nesting level deeper, a sentinel literal the cleaner and the
// validator disagreed about, a user tag shadowing a core built-in, and a target
// kind (inbound) no check looked at. Each one is the incident class again — a
// config the core will reject, emitted by a pipeline that believed it was clean.

// minCfg builds a config with a usable final and two real outbounds, so tests can
// add exactly the reference under test without tripping the other checks.
func minCfg(t *testing.T, outbounds string, route string) map[string]interface{} {
	t.Helper()
	raw := `{
		"outbounds": [` + outbounds + `],
		"route": ` + route + `
	}`
	return decodeCfg(t, raw)
}

const (
	baseOutbounds = `{"type":"selector","tag":"proxy","outbounds":["direct-out"]},
	                 {"type":"direct","tag":"direct-out"}`
	plainRoute = `{"final":"proxy"}`
)

// TestNestedLogicalRuleTargetIsCaught — a dangling `outbound` inside a logical
// rule used to be invisible.
//
// `{"type":"logical","mode":"or","rules":[...]}` is a first-class sing-box rule
// form and this repo generates it, but both the cleaner and the validator
// iterated only the top level of `rules`. A dangling target one level down was
// therefore emitted unchecked AND passed validation — the exact production
// failure, just nested.
func TestNestedLogicalRuleTargetIsCaught(t *testing.T) {
	cfg := minCfg(t, baseOutbounds, `{
		"final": "proxy",
		"rules": [
			{"type":"logical","mode":"or","rules":[
				{"domain":"a.com","outbound":"ghost-out"}
			]}
		]
	}`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a dangling outbound nested inside a logical rule must be caught; " +
			"reporting this config as clean is the original incident one level down")
	}
	// The path must locate the rule, or the report is not actionable.
	if !strings.Contains(rep.Error(), "rules[0].rules[0]") {
		t.Errorf("the issue must point at the nested rule, got: %s", rep.Error())
	}
}

// TestDeeplyNestedRuleTargetIsCaught — the walk recurses, it does not just look
// one level deeper.
func TestDeeplyNestedRuleTargetIsCaught(t *testing.T) {
	cfg := minCfg(t, baseOutbounds, `{
		"final": "proxy",
		"rules": [
			{"type":"logical","mode":"and","rules":[
				{"type":"logical","mode":"or","rules":[
					{"type":"logical","mode":"and","rules":[
						{"domain":"deep.com","outbound":"ghost-deep"}
					]}
				]}
			]}
		]
	}`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("recursion must reach any depth, not just one level")
	}
	if !strings.Contains(rep.Error(), "ghost-deep") {
		t.Errorf("the issue must name the dangling tag, got: %s", rep.Error())
	}
}

// TestNestedDNSRuleServerIsCaught — the same nesting blindness in dns.rules.
func TestNestedDNSRuleServerIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy"},
		"dns": {
			"servers": [{"type":"udp","tag":"dns-local","server":"1.1.1.1"}],
			"rules": [
				{"type":"logical","mode":"or","rules":[
					{"domain":"a.com","server":"ghost-dns"}
				]}
			]
		}
	}`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a dangling DNS server nested inside a logical rule must be caught")
	}
	if !strings.Contains(rep.Error(), "ghost-dns") {
		t.Errorf("the issue must name the dangling server, got: %s", rep.Error())
	}
}

// TestCleanerRepairsNestedRule — the cleaner must recurse too, otherwise it
// leaves nested dangling targets for the validator to reject: a build that could
// have been repaired now fails.
func TestCleanerRepairsNestedRule(t *testing.T) {
	route := json.RawMessage(`{
		"final":"direct-out",
		"rules":[
			{"type":"logical","mode":"or","rules":[
				{"domain":"x.com","outbound":"ghost"}
			]}
		]
	}`)
	out, warns, err := CleanDanglingOutboundsInRouteRules(route, map[string]bool{"direct-out": true}, "direct-out")
	if err != nil {
		t.Fatalf("a nested dangling target with a valid fallback must be repairable, got: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("the repair must be reported as a warning, got %v", warns)
	}
	// The repair must land on the NESTED rule, not the outer one.
	if !strings.Contains(string(out), `"outbound": "direct-out"`) {
		t.Errorf("the nested rule must be rewired to the fallback, got: %s", string(out))
	}
	if strings.Contains(string(out), "ghost") {
		t.Errorf("no dangling tag may survive the repair, got: %s", string(out))
	}
}

// TestSentinelLiteralsAreNotDangling — the cleaner deliberately leaves
// `reject`/`block`/`drop`/`direct`/`dns-out` alone as literals the core resolves,
// but the validator had no sentinel concept and reported them missing.
//
// The two halves of one build therefore disagreed about the same value: a config
// the core accepts failed to build, and the error claimed a dangling reference
// that did not exist.
func TestSentinelLiteralsAreNotDangling(t *testing.T) {
	for _, sentinel := range []string{"reject", "block", "drop", "direct", "dns-out"} {
		cfg := minCfg(t, baseOutbounds, `{
			"final":"proxy",
			"rules":[{"domain":"a.com","outbound":"`+sentinel+`"}]
		}`)
		rep := ValidateConfigReferences(cfg)
		if !rep.OK() {
			t.Errorf("sentinel literal %q is resolved by the core and must not be "+
				"reported as a dangling reference, got: %s", sentinel, rep.Error())
		}
	}
}

// TestReservedTagCollisionIsCaught — a user outbound reusing `direct`/`block`/
// `dns-out` makes every reference to that tag ambiguous, and the config used to
// validate as clean.
func TestReservedTagCollisionIsCaught(t *testing.T) {
	for _, reserved := range []string{"direct", "block", "dns-out"} {
		cfg := minCfg(t,
			`{"type":"selector","tag":"proxy","outbounds":["direct-out"]},
			 {"type":"direct","tag":"direct-out"},
			 {"type":"socks","tag":"`+reserved+`","server":"1.2.3.4","server_port":1080}`,
			plainRoute)
		rep := ValidateConfigReferences(cfg)
		if rep.OK() {
			t.Errorf("user outbound tagged %q collides with a core built-in and must "+
				"be rejected; the reference `outbound:%s` is otherwise ambiguous", reserved, reserved)
		}
	}
}

// TestInboundReferenceIsCaught — `route.rules[*].inbound` names an inbound tag,
// and template #if branches can drop an inbound, so the target is removable. No
// check looked at it at all.
func TestInboundReferenceIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
		"inbounds": [{"type":"tun","tag":"tun-in"}],
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy","rules":[{"inbound":"ghost-in","outbound":"direct-out"}]}
	}`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a rule targeting a non-existent inbound must be caught")
	}
	if !strings.Contains(rep.Error(), "inbound") || !strings.Contains(rep.Error(), "ghost-in") {
		t.Errorf("the issue must identify the inbound reference, got: %s", rep.Error())
	}
}

// TestValidInboundReferenceIsAccepted — no false positive when the inbound exists.
func TestValidInboundReferenceIsAccepted(t *testing.T) {
	cfg := decodeCfg(t, `{
		"inbounds": [{"type":"tun","tag":"tun-in"}],
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy","rules":[{"inbound":"tun-in","outbound":"direct-out"}]}
	}`)
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("a rule naming a declared inbound is valid, got: %s", rep.Error())
	}
}

// TestInboundNestedInLogicalRuleIsCaught — the inbound check participates in the
// recursion, rather than only being applied at the top level.
func TestInboundNestedInLogicalRuleIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
		"inbounds": [{"type":"tun","tag":"tun-in"}],
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy","rules":[
			{"type":"logical","rules":[{"inbound":"ghost-in","outbound":"direct-out"}]}
		]}
	}`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("the inbound check must apply at every depth like the others")
	}
}

// TestNoInboundsDeclaredSkipsInboundCheck — a config with no inbounds section
// (a minimal template) must not have every inbound-tagged rule reported.
func TestNoInboundsDeclaredSkipsInboundCheck(t *testing.T) {
	cfg := minCfg(t, baseOutbounds, `{
		"final":"proxy",
		"rules":[{"inbound":"something","outbound":"direct-out"}]
	}`)
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("with no inbounds declared there is nothing to resolve against, got: %s", rep.Error())
	}
}

// TestDNSFinalWithoutServersIsNotReported — dns.final previously lacked the
// "no targets declared" escape hatch that route.final has, so a minimal template
// naming a final with no server list was rejected where the route twin was
// deliberately tolerated.
func TestDNSFinalWithoutServersIsNotReported(t *testing.T) {
	cfg := decodeCfg(t, `{
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy"},
		"dns": {"final":"dns-local"}
	}`)
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("dns.final with no declared servers has nothing to resolve against, got: %s", rep.Error())
	}
}

// TestDNSFinalWithServersIsStillCaught — closing the false positive must not open
// a hole: when servers ARE declared, a dangling final is still an error.
func TestDNSFinalWithServersIsStillCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
		"outbounds": [`+baseOutbounds+`],
		"route": {"final":"proxy"},
		"dns": {
			"servers": [{"type":"udp","tag":"dns-local","server":"1.1.1.1"}],
			"final": "ghost-dns"
		}
	}`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a dangling dns.final must still be caught when servers are declared")
	}
}

// TestDanglingWithoutFallbackFailsRatherThanDroppingTheRule is the regression for
// silent semantic loss: the cleaner deleted a user's routing rule when it could
// not be repaired, rerouting the traffic through route.final with only a log line
// to show for it.
func TestDanglingWithoutFallbackFailsRatherThanDroppingTheRule(t *testing.T) {
	route := json.RawMessage(`{
		"rules": [{"domain":"bank.com","outbound":"ru-out"}]
	}`)
	out, _, err := CleanDanglingOutboundsInRouteRules(route, map[string]bool{"direct-out": true}, "")
	if err == nil {
		t.Fatalf("an unrepairable dangling target must fail the build, not silently "+
			"delete the rule; got %s", string(out))
	}
	if !strings.Contains(err.Error(), "ru-out") {
		t.Errorf("the error must name the dangling tag so it can be fixed, got: %v", err)
	}
}

// TestSentinelRuleSurvivesCleaningUnchanged — the cleaner's sentinel contract is
// what the validator now agrees with; pin it so the two cannot drift apart again.
func TestSentinelRuleSurvivesCleaningUnchanged(t *testing.T) {
	route := json.RawMessage(`{
		"rules": [{"domain":"a.com","outbound":"block"}]
	}`)
	out, warns, err := CleanDanglingOutboundsInRouteRules(route, map[string]bool{"direct-out": true}, "")
	if err != nil {
		t.Fatalf("a sentinel literal is legal and must not fail: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("a sentinel literal needs no repair, got warnings %v", warns)
	}
	if !strings.Contains(string(out), `"block"`) {
		t.Errorf("the sentinel must survive unchanged, got: %s", string(out))
	}
}
