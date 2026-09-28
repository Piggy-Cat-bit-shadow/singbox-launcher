package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/core/template"
)

// decodeCfg parses a config literal for reference tests.
func decodeCfg(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return cfg
}

// TestRouteFinalDanglingIsCaught is the regression for the real production
// failure: `default outbound not found: proxy-out`.
//
// The config below is exactly the shape that reached the core — route.final
// names a tag that no outbound carries. Before the integrity check existed,
// BuildConfig returned this happily, the file was written, and the core refused
// to start with an error that pointed at the outbound rather than at the
// generator that produced it.
func TestRouteFinalDanglingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "🌍 国外流量", "outbounds": ["node-a", "direct-out"]},
        {"type": "shadowsocks", "tag": "node-a"},
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)

	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a route.final naming a non-existent outbound must be reported")
	}
	if !strings.Contains(rep.Error(), "route.final") || !strings.Contains(rep.Error(), "proxy-out") {
		t.Fatalf("report must name route.final and the missing tag, got: %s", rep.Error())
	}
}

// TestRepairRouteFinalPicksSurvivingGroup — when the builder CAN fix it, it must
// pick deterministically, preferring a real traffic group over direct/block.
func TestRepairRouteFinalPicksSurvivingGroup(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "block", "tag": "block-out"},
        {"type": "selector", "tag": "🌍 国外流量", "outbounds": ["direct-out"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)

	repairs, ok := RepairRouteFinal(cfg, nil)
	if !ok {
		t.Fatal("repair must succeed when a group survives")
	}
	if len(repairs) != 1 {
		t.Fatalf("expected exactly one repair record, got %v", repairs)
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "🌍 国外流量" {
		t.Fatalf("final must be the surviving selector, got %v", got)
	}
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("repaired config must be clean, got: %s", rep.Error())
	}
}

// TestRepairRouteFinalIsDeterministic — same input, same output, every time.
// Map iteration order must not leak into the emitted config.
func TestRepairRouteFinalIsDeterministic(t *testing.T) {
	fixture := `{
      "outbounds": [
        {"type": "selector", "tag": "z-group", "outbounds": ["direct-out"]},
        {"type": "selector", "tag": "a-group", "outbounds": ["direct-out"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "gone", "rules": []}
    }`
	first := ""
	for i := 0; i < 30; i++ {
		cfg := decodeCfg(t, fixture)
		if _, ok := RepairRouteFinal(cfg, nil); !ok {
			t.Fatal("repair failed")
		}
		got := cfg["route"].(map[string]interface{})["final"].(string)
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("repair is non-deterministic: %q then %q", first, got)
		}
	}
	// Declaration order decides: z-group is declared first.
	if first != "z-group" {
		t.Fatalf("expected first declared group, got %q", first)
	}
}

// TestRepairRouteFinalFallsBackToDirect — with no group left, direct/block is
// still better than an unroutable config.
func TestRepairRouteFinalFallsBackToDirect(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)
	if _, ok := RepairRouteFinal(cfg, nil); !ok {
		t.Fatal("repair must succeed via direct fallback")
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "direct-out" {
		t.Fatalf("expected direct-out fallback, got %v", got)
	}
}

// TestRepairRouteFinalRefusesWhenNothingCanCarryTraffic — outbounds exist but
// none is usable. This must NOT be silently "repaired"; there is no valid answer.
func TestRepairRouteFinalRefusesWhenNothingCanCarryTraffic(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "shadowsocks", "tag": "node-a"}],
      "route": {"final": "proxy-out", "rules": []}
    }`)
	if _, ok := RepairRouteFinal(cfg, nil); ok {
		t.Fatal("a config with no traffic-capable outbound must not be reported as repairable")
	}
}

// TestRouteFinalUntouchedWhenValid — the validator must not "helpfully" rewrite
// a config that is already correct.
func TestRouteFinalUntouchedWhenValid(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": ["direct-out"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "grp", "rules": []}
    }`)
	repairs, ok := RepairRouteFinal(cfg, nil)
	if !ok || len(repairs) != 0 {
		t.Fatalf("a valid final must not be repaired, got %v ok=%v", repairs, ok)
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "grp" {
		t.Fatalf("valid final was changed to %v", got)
	}
}

// TestRouteFinalAbsentIsNotRepaired — a config with no catch-all at all is the
// template's choice, not a dangling reference.
func TestRouteFinalAbsentIsNotRepaired(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "route": {"rules": []}
    }`)
	repairs, ok := RepairRouteFinal(cfg, nil)
	if !ok || len(repairs) != 0 {
		t.Fatalf("absent final must be left alone, got %v", repairs)
	}
}

// TestSelectorMemberFilteredIsCaught — the same class of bug, one level down:
// a group that still lists a tag that was filtered out.
func TestSelectorMemberFilteredIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": ["node-a", "node-gone"]},
        {"type": "shadowsocks", "tag": "node-a"}
      ],
      "route": {"final": "grp", "rules": []}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a selector listing a filtered-out member must be reported")
	}
	if !strings.Contains(rep.Error(), "node-gone") {
		t.Fatalf("report must name the filtered member, got: %s", rep.Error())
	}
}

// TestEmptySelectorIsCaught — every member filtered away leaves a group the core
// will refuse to load.
func TestEmptySelectorIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": []},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "direct-out", "rules": []}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("an empty selector must be reported")
	}
	if !strings.Contains(rep.Error(), "no members") {
		t.Fatalf("report must explain the group is empty, got: %s", rep.Error())
	}
}

// TestEmptyURLTestIsCaught — same invariant for urltest.
func TestEmptyURLTestIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "urltest", "tag": "auto", "outbounds": []},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("an empty urltest must be reported")
	}
}

// TestURLTestMemberFilteredIsCaught — urltest members are the same class.
func TestURLTestMemberFilteredIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "urltest", "tag": "auto", "outbounds": ["node-gone"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a urltest listing a filtered member must be reported")
	}
}

// TestChainMemberFilteredIsCaught — a chain whose member disappeared is just as
// dangling as a selector's.
func TestChainMemberFilteredIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "chain", "tag": "chained", "outbounds": ["node-a"], "detour": "node-gone"},
        {"type": "shadowsocks", "tag": "node-a"}
      ],
      "route": {"final": "chained", "rules": []}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a chain with a filtered detour must be reported")
	}
	if !strings.Contains(rep.Error(), "node-gone") {
		t.Fatalf("report must name the missing chain member, got: %s", rep.Error())
	}
}

// TestOutboundDetourMissingIsCaught — a plain outbound's detour target.
func TestOutboundDetourMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "shadowsocks", "tag": "node-a", "detour": "gone"},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a missing outbound detour must be reported")
	}
}

// TestRouteRuleOutboundMissingIsCaught — the route-rule form of the same bug.
func TestRouteRuleOutboundMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": ["direct-out"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "grp", "rules": [{"domain": ["x.test"], "outbound": "gone"}]}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a route rule targeting a missing outbound must be reported")
	}
	if !strings.Contains(rep.Error(), "route.rules[0].outbound") {
		t.Fatalf("report must locate the rule, got: %s", rep.Error())
	}
}

// TestDNSDetourMissingIsCaught — DNS server detours are outbound references.
func TestDNSDetourMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {"servers": [{"tag": "s", "type": "udp", "server": "1.1.1.1", "detour": "gone"}]},
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a missing DNS detour must be reported")
	}
}

// TestDNSRuleServerMissingIsCaught — a DNS rule pointing at an absent server.
func TestDNSRuleServerMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {
        "servers": [{"tag": "s", "type": "local"}],
        "rules": [{"domain": ["x.test"], "server": "gone"}]
      },
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a DNS rule naming a missing server must be reported")
	}
}

// TestDNSFinalMissingIsCaught — dns.final is the DNS analogue of route.final and
// was the same blind spot.
func TestDNSFinalMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {"servers": [{"tag": "s", "type": "local"}], "final": "gone"},
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); rep.OK() {
		t.Fatal("a missing dns.final must be reported")
	}
}

// TestDomainResolverIsValidatedAgainstServerTags guards against a FALSE POSITIVE
// I introduced and then had to fix: `domain_resolver` names a DNS SERVER tag,
// not an outbound. Checking it against the outbound set rejected correct
// configs — and a validator that cries wolf gets switched off.
func TestDomainResolverIsValidatedAgainstServerTags(t *testing.T) {
	// The resolver names a server tag that is NOT an outbound: valid.
	valid := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {"servers": [
        {"tag": "direct_dns_resolver", "type": "local"},
        {"tag": "remote", "type": "udp", "server": "1.1.1.1", "domain_resolver": "direct_dns_resolver"}
      ]},
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(valid); !rep.OK() {
		t.Fatalf("domain_resolver naming a server tag must be accepted, got: %s", rep.Error())
	}

	// A resolver naming nothing at all is a real dangling reference.
	broken := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {"servers": [
        {"tag": "remote", "type": "udp", "server": "1.1.1.1", "domain_resolver": "gone"}
      ]},
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(broken); rep.OK() {
		t.Fatal("domain_resolver naming a missing server must be reported")
	}
}

// TestForwardReferencedDomainResolverIsAccepted — a server may resolve through a
// server declared later; the check must not depend on declaration order.
func TestForwardReferencedDomainResolverIsAccepted(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "dns": {"servers": [
        {"tag": "remote", "type": "udp", "server": "1.1.1.1", "domain_resolver": "resolver"},
        {"tag": "resolver", "type": "local"}
      ]},
      "route": {"final": "direct-out", "rules": []}
    }`)
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("forward-referenced resolver must be accepted, got: %s", rep.Error())
	}
}

// TestRouteRuleRuleSetMissingIsCaught — rule_set references are tag references.
func TestRouteRuleRuleSetMissingIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "route": {
        "final": "direct-out",
        "rule_set": [{"tag": "declared", "type": "inline", "format": "domain_suffix", "rules": [{"domain_suffix": ["ru"]}]}],
        "rules": [{"rule_set": ["declared"]}]
      },
      "dns": {"rules": [{"rule_set": ["ghost"]}]}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a rule_set reference to an undeclared set must be reported")
	}
	if !strings.Contains(rep.Error(), "ghost") {
		t.Fatalf("report must name the undeclared set, got: %s", rep.Error())
	}
}

// TestDuplicateTagIsCaught — with two outbounds sharing a tag, every reference
// to it is ambiguous, so the condition is reported on its own terms.
func TestDuplicateTagIsCaught(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "direct", "tag": "dup"},
        {"type": "block", "tag": "dup"}
      ],
      "route": {"final": "dup", "rules": []}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("a duplicate tag must be reported")
	}
	if !strings.Contains(rep.Error(), "more than once") {
		t.Fatalf("report must explain the duplication, got: %s", rep.Error())
	}
}

// TestCleanConfigHasNoIssues — the negative control. Without this, a validator
// that reports everything would look like it works.
func TestCleanConfigHasNoIssues(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": ["node-a", "auto", "direct-out"]},
        {"type": "urltest", "tag": "auto", "outbounds": ["node-a"]},
        {"type": "shadowsocks", "tag": "node-a", "detour": "direct-out"},
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "dns": {
        "servers": [
          {"tag": "resolver", "type": "local"},
          {"tag": "remote", "type": "udp", "server": "1.1.1.1", "detour": "direct-out", "domain_resolver": "resolver"}
        ],
        "final": "remote",
        "rules": [{"rule_set": ["ads"], "server": "resolver"}]
      },
      "route": {
        "final": "grp",
        "rule_set": [{"tag": "ads", "type": "inline", "format": "domain_suffix", "rules": [{"domain_suffix": ["ads.test"]}]}],
        "rules": [{"rule_set": ["ads"], "outbound": "block-out"}]
      }
    }`)
	if rep := ValidateConfigReferences(cfg); !rep.OK() {
		t.Fatalf("a fully-resolvable config must produce no issues, got: %s", rep.Error())
	}
}

// TestValidateConfigBytesRejectsUnparseable — "could not check" must not be
// mistaken for "is fine".
func TestValidateConfigBytesRejectsUnparseable(t *testing.T) {
	if _, err := ValidateConfigBytes([]byte("{not json")); err == nil {
		t.Fatal("unparseable config must return an error, not an empty report")
	}
}

// TestValidateConfigBytesAcceptsComments — the builder emits `//` comments, so
// the validator must parse JSONC or it will reject every real config.
func TestValidateConfigBytesAcceptsComments(t *testing.T) {
	raw := []byte(`{
      // generated by the template
      "outbounds": [{"type": "direct", "tag": "direct-out"}],
      "route": {"final": "direct-out", "rules": []} // catch-all
    }`)
	rep, err := ValidateConfigBytes(raw)
	if err != nil {
		t.Fatalf("JSONC config must parse: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("commented config is valid, got: %s", rep.Error())
	}
}

// TestReportListsEveryIssue — one error per build attempt is unusable when a
// user has several broken references; all of them must be reported at once.
func TestReportListsEveryIssue(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "selector", "tag": "grp", "outbounds": ["gone-member"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "gone-final", "rules": [{"outbound": "gone-rule"}]}
    }`)
	rep := ValidateConfigReferences(cfg)
	if rep.OK() {
		t.Fatal("expected issues")
	}
	for _, want := range []string{"gone-member", "gone-final", "gone-rule"} {
		if !strings.Contains(rep.Error(), want) {
			t.Errorf("report must include %q, got: %s", want, rep.Error())
		}
	}
}

// TestRepairRefusesToRedirectAGroupToDirect is the regression for the incident's
// second half.
//
// When the template declared the vanished `route.final` target as a GROUP, the
// repair used to fall back to `direct-out`. That config STARTS, VALIDATES, and
// silently sends every unmatched connection straight out — no tunnel, no error,
// and no symptom beyond a warning line. A user who believes the VPN is on is
// worse off than one who is told it cannot start.
//
// The declaration is what distinguishes this from the legitimate fallback in
// TestRepairRouteFinalFallsBackToDirect: there, nothing says the vanished tag was
// a routing CHOICE rather than a single server.
func TestRepairRefusesToRedirectAGroupToDirect(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)
	// The template said proxy-out was a required group. It is gone, and no group
	// survives to take its place.
	declared := map[string]bool{"proxy-out": true}

	repairs, ok := RepairRouteFinal(cfg, declared)
	if ok {
		t.Fatalf("repair must REFUSE rather than redirect traffic to a direct "+
			"outbound; got repairs=%v final=%v", repairs,
			cfg["route"].(map[string]interface{})["final"])
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "proxy-out" {
		t.Errorf("a refused repair must not modify the config, got final=%v", got)
	}
}

// TestRepairStillRedirectsANonGroupNode pins that the refusal is scoped to
// GROUPS. A vanished plain node was one server, not a routing choice, so letting
// the catch-all fall to whatever routes is still the right repair.
func TestRepairStillRedirectsANonGroupNode(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "selector", "tag": "grp", "outbounds": ["direct-out"]}
      ],
      "route": {"final": "node-gone", "rules": []}
    }`)
	declared := map[string]bool{"node-gone": true} // declared, but NOT as a group

	repairs, ok := RepairRouteFinal(cfg, declared)
	if !ok {
		t.Fatal("a vanished non-group node must still be repairable")
	}
	if len(repairs) != 1 {
		t.Fatalf("expected one repair record, got %v", repairs)
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "grp" {
		t.Fatalf("expected the surviving group, got %v", got)
	}
}

// TestRepairPrefersASurvivingGroupOverTheDeclarationCheck — when a group DOES
// survive, the repair proceeds even though the original was a group.
func TestRepairPrefersASurvivingGroupOverTheDeclarationCheck(t *testing.T) {
	cfg := decodeCfg(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "selector", "tag": "🌍 国外流量", "outbounds": ["direct-out"]}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)
	repairs, ok := RepairRouteFinal(cfg, map[string]bool{"proxy-out": true})
	if !ok || len(repairs) != 1 {
		t.Fatalf("a surviving group must still be used, got ok=%v repairs=%v", ok, repairs)
	}
	if got := cfg["route"].(map[string]interface{})["final"]; got != "🌍 国外流量" {
		t.Fatalf("expected the surviving group, got %v", got)
	}
}

// TestDeclaredGroupTagsReadsRequiredGroups proves the build can actually obtain
// the declaration it now relies on, from the SHIPPING template.
//
// The accessor (template.RequiredOutboundTags) existed but had NO production
// caller, so `required: true` was parsed, carried around, and consulted by
// nothing. This pins that the wiring is real and reads the real file.
func TestDeclaredGroupTagsReadsRequiredGroups(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "bin", "wizard_template.json"))
	if err != nil {
		t.Fatalf("read shipping template: %v", err)
	}
	td, err := template.ParseTemplateData(raw)
	if err != nil {
		t.Fatalf("parse shipping template: %v", err)
	}
	groups := declaredGroupTags(td)
	if len(groups) == 0 {
		t.Fatal("no declared groups found; the shipping template declares proxy-out " +
			"as required, so this must not be empty")
	}
	if !groups["proxy-out"] {
		t.Errorf("proxy-out is declared required=true but was not reported; got %v", groups)
	}
}
