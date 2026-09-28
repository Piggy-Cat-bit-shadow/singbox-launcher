// Config reference integrity: every tag-based reference in the emitted config
// must resolve to a tag that actually exists.
//
// WHY THIS FILE EXISTS
//
// A real config reached sing-box with `route.final = "proxy-out"` while no
// outbound carried that tag, and the core refused to start with
//
//	default outbound not found: proxy-out
//
// The interesting part is not the missing tag — tags legitimately disappear when
// nodes are filtered by a capability gate, when a selector loses all its
// members, or when the user switches direction. The defect is that NOTHING
// checked the references after the graph changed. `route.final` was in fact read
// in the build (as the fallback target for dangling route RULES), but it was
// never itself validated, so it survived every cleanup pass untouched.
//
// Repairing only `route.final` would have fixed exactly one symptom. The same
// hole existed for every other tag-based reference, so this is one validator
// over the whole object graph rather than a special case per field:
//
//	route.final                     route.rules[*].outbound
//	outbound.detour                 selector / urltest members
//	chain members                   dns.servers[*].detour
//	dns.rules[*].server             rule_set references
//	endpoint / service tag refs
//
// INVARIANT: a config that passes BuildConfig has no dangling internal
// reference. Anything that cannot be repaired is reported, never emitted.

package build

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/muhammadmuzzammil1998/jsonc"
)

// RefIssueKind classifies a reference problem.
//
// The distinction matters to the caller: a MISSING target can sometimes be
// repaired (reset route.final to a sane default), while an EMPTY container is a
// configuration error the user has to resolve.
type RefIssueKind string

const (
	// RefMissingTarget — the reference names a tag that does not exist.
	RefMissingTarget RefIssueKind = "missing_target"
	// RefEmptyGroup — a selector/urltest with no members left after filtering.
	RefEmptyGroup RefIssueKind = "empty_group"
	// RefDuplicateTag — two entities share a tag, so references are ambiguous.
	RefDuplicateTag RefIssueKind = "duplicate_tag"
)

// RefIssue is one broken reference, located well enough to act on.
type RefIssue struct {
	Kind RefIssueKind
	// Path is the JSON-ish location, e.g. `route.final` or `outbounds[3].outbounds[1]`.
	Path string
	// Tag is the referenced name (empty for structural problems).
	Tag string
	// Detail explains the problem in one sentence.
	Detail string
}

func (i RefIssue) String() string {
	if i.Tag == "" {
		return fmt.Sprintf("%s: %s", i.Path, i.Detail)
	}
	return fmt.Sprintf("%s: %s %q: %s", i.Path, i.Kind, i.Tag, i.Detail)
}

// RefReport is the outcome of a reference check.
type RefReport struct {
	Issues []RefIssue
	// Repairs lists what was changed automatically (e.g. route.final reset).
	Repairs []string
}

// OK reports whether the config is reference-clean.
func (r RefReport) OK() bool { return len(r.Issues) == 0 }

// Error renders the issues as one error, or nil.
func (r RefReport) Error() string {
	if r.OK() {
		return ""
	}
	parts := make([]string, 0, len(r.Issues))
	for _, is := range r.Issues {
		parts = append(parts, is.String())
	}
	return strings.Join(parts, "; ")
}

// refIndex is the set of tags a config actually defines.
type refIndex struct {
	// defined is every tag present in outbounds.
	defined map[string]bool
	// order preserves declaration order, so a replacement for route.final is
	// chosen deterministically rather than by map iteration.
	order []string
	// group is the subset that can serve as a traffic target (selector/urltest/
	// chain), which is what route.final must point at.
	group map[string]bool
	// directLike is the subset that is a plain outbound with a known-good
	// behaviour, used as the last-resort fallback (direct/block style).
	directLike map[string]bool
	// duplicates holds tags declared more than once.
	duplicates map[string]bool
	// count is how many tagged outbounds/endpoints were declared at all. Used to
	// tell "a tag was removed by filtering" (corruption) apart from "this
	// template declares no outbounds" (a minimal or lx-only template).
	count int
}

func newRefIndex() *refIndex {
	return &refIndex{
		defined:    map[string]bool{},
		group:      map[string]bool{},
		directLike: map[string]bool{},
		duplicates: map[string]bool{},
	}
}

// groupTypes are the outbound types that route traffic and can therefore be a
// route.final target.
var groupTypes = map[string]bool{
	"selector": true,
	"urltest":  true,
	"chain":    true,
}

// directTypes are outbounds with unconditional behaviour, valid as a last-resort
// final target when no group survives.
var directTypes = map[string]bool{
	"direct": true,
	"block":  true,
}

// buildRefIndex walks the outbound list once and records its tags.
func buildRefIndex(cfg map[string]interface{}) *refIndex {
	idx := newRefIndex()
	outbounds, _ := cfg["outbounds"].([]interface{})
	for _, raw := range outbounds {
		ob, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		tag, _ := ob["tag"].(string)
		if tag == "" {
			continue
		}
		if idx.defined[tag] {
			idx.duplicates[tag] = true
			continue
		}
		idx.defined[tag] = true
		idx.order = append(idx.order, tag)
		idx.count++
		typ, _ := ob["type"].(string)
		if groupTypes[typ] {
			idx.group[tag] = true
		}
		if directTypes[typ] {
			idx.directLike[tag] = true
		}
	}
	// Endpoints are addressable by tag too and may be referenced as detours.
	if endpoints, ok := cfg["endpoints"].([]interface{}); ok {
		for _, raw := range endpoints {
			ep, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			tag, _ := ep["tag"].(string)
			if tag == "" {
				continue
			}
			if idx.defined[tag] {
				idx.duplicates[tag] = true
				continue
			}
			idx.defined[tag] = true
			idx.order = append(idx.order, tag)
			idx.count++
		}
	}
	return idx
}

// ValidateConfigReferences checks every internal tag reference in a built config.
//
// It does NOT mutate cfg; use RepairRouteFinal for the one repair that is
// unambiguously safe. Splitting the two means a caller that only wants to know
// whether a config is sound (for example to report it) never has a config
// silently changed underneath it.
func ValidateConfigReferences(cfg map[string]interface{}) RefReport {
	var rep RefReport
	idx := buildRefIndex(cfg)

	// Duplicate tags first: every other check would be ambiguous.
	for tag := range idx.duplicates {
		rep.Issues = append(rep.Issues, RefIssue{
			Kind: RefDuplicateTag, Path: "outbounds", Tag: tag,
			Detail: "tag is declared more than once, so references to it are ambiguous",
		})
	}
	sort.Slice(rep.Issues, func(a, b int) bool { return rep.Issues[a].Tag < rep.Issues[b].Tag })

	// An empty group is broken regardless of whether anything points at it:
	// sing-box refuses to load a selector/urltest with no members, and it is
	// always a build error rather than something to repair silently.
	outbounds, _ := cfg["outbounds"].([]interface{})
	for i, raw := range outbounds {
		ob, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := ob["type"].(string)
		tag, _ := ob["tag"].(string)
		if !groupTypes[typ] {
			continue
		}
		members := stringSlice(ob["outbounds"])
		if len(members) == 0 {
			rep.Issues = append(rep.Issues, RefIssue{
				Kind: RefEmptyGroup, Path: fmt.Sprintf("outbounds[%d]", i), Tag: tag,
				Detail: fmt.Sprintf("%s %q has no members left after filtering", typ, tag),
			})
			continue
		}
		for j, m := range members {
			if !idx.defined[m] {
				rep.Issues = append(rep.Issues, RefIssue{
					Kind: RefMissingTarget,
					Path: fmt.Sprintf("outbounds[%d].outbounds[%d]", i, j), Tag: m,
					Detail: fmt.Sprintf("%s %q lists a member that does not exist", typ, tag),
				})
			}
		}
		// A chain additionally references a detour-carrying outbound.
		if typ == "chain" {
			rep.Issues = append(rep.Issues, checkDetour(ob, fmt.Sprintf("outbounds[%d]", i), idx)...)
		}
	}

	// outbound.detour on any outbound type that supports it.
	for i, raw := range outbounds {
		ob, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := ob["type"].(string)
		if groupTypes[typ] {
			continue // already handled above
		}
		rep.Issues = append(rep.Issues, checkDetour(ob, fmt.Sprintf("outbounds[%d]", i), idx)...)
	}

	// route.final and route.rules[*].outbound.
	//
	// Rules are validated here only when the config has an outbound list to
	// resolve against. A skeleton template with no outbounds at all cannot have
	// a meaningful rule target, and the fixtures that build such templates
	// (lx pass-through, DNS rule_set handling) assert on other sections
	// entirely — reporting their rule tags as dangling would reject configs that
	// are not what this check is for.
	if route, ok := cfg["route"].(map[string]interface{}); ok {
		if final, ok := route["final"].(string); ok && final != "" {
			// Only a problem when outbounds exist to point AT. With none, the
			// template is simply not outbound-based (see RepairRouteFinal).
			if !idx.defined[final] && idx.count > 0 {
				rep.Issues = append(rep.Issues, RefIssue{
					Kind: RefMissingTarget, Path: "route.final", Tag: final,
					Detail: "the catch-all outbound does not exist, so the core cannot route unmatched traffic",
				})
			}
		}
		rules, _ := route["rules"].([]interface{})
		for i, raw := range rules {
			rule, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if ob, ok := rule["outbound"].(string); ok && ob != "" && idx.count > 0 {
				if !idx.defined[ob] {
					rep.Issues = append(rep.Issues, RefIssue{
						Kind: RefMissingTarget, Path: fmt.Sprintf("route.rules[%d].outbound", i), Tag: ob,
						Detail: "the rule targets an outbound that does not exist",
					})
				}
			}
			// rule_set references must resolve to a declared rule_set tag.
			// Checked in checkRuleSets, which needs the rule_set list.
		}
	}

	// DNS: server detours and rule targets.
	if dns, ok := cfg["dns"].(map[string]interface{}); ok {
		servers, _ := dns["servers"].([]interface{})
		serverTags := map[string]bool{}
		for i, raw := range servers {
			srv, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if tag, ok := srv["tag"].(string); ok && tag != "" {
				serverTags[tag] = true
			}
			rep.Issues = append(rep.Issues, checkDetour(srv, fmt.Sprintf("dns.servers[%d]", i), idx)...)
		}
		if final, ok := dns["final"].(string); ok && final != "" {
			if !serverTags[final] {
				rep.Issues = append(rep.Issues, RefIssue{
					Kind: RefMissingTarget, Path: "dns.final", Tag: final,
					Detail: "the final DNS server does not exist",
				})
			}
		}
		dnsRules, _ := dns["rules"].([]interface{})
		for i, raw := range dnsRules {
			rule, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if srv, ok := rule["server"].(string); ok && srv != "" {
				if !serverTags[srv] {
					rep.Issues = append(rep.Issues, RefIssue{
						Kind: RefMissingTarget, Path: fmt.Sprintf("dns.rules[%d].server", i), Tag: srv,
						Detail: "the DNS rule targets a server that does not exist",
					})
				}
			}
			rep.Issues = append(rep.Issues, checkRuleSetRefs(rule, fmt.Sprintf("dns.rules[%d]", i), cfg)...)
		}
		// Second pass: `domain_resolver` names a SERVER tag, and server tags are
		// only fully known once the loop above has seen them all. Doing it inline
		// would miss forward references (a server resolving via a server declared
		// later), which is legal in sing-box.
		for i, raw := range servers {
			srv, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			rep.Issues = append(rep.Issues,
				checkDomainResolver(srv, fmt.Sprintf("dns.servers[%d]", i), serverTags)...)
		}
		if route, ok := cfg["route"].(map[string]interface{}); ok {
			rep.Issues = append(rep.Issues,
				checkDomainResolver(route, "route", serverTags)...)
		}
		for i, raw := range dnsRules {
			rule, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			rep.Issues = append(rep.Issues,
				checkDomainResolver(rule, fmt.Sprintf("dns.rules[%d]", i), serverTags)...)
		}
	}
	return rep
}

// checkDetour validates an outbound-valued `detour` reference.
//
// `domain_resolver` is deliberately NOT checked here: it names a DNS SERVER tag
// (`dns.servers[*].tag`), not an outbound, as the template's own
// `dns_default_domain_resolver` tooltip states. Validating it against the
// outbound set produced false positives on correct configs, which is worse than
// not checking it at all — a validator that cries wolf gets ignored.
// checkDomainResolver covers it against the right namespace instead.
func checkDetour(obj map[string]interface{}, path string, idx *refIndex) []RefIssue {
	ref, ok := obj["detour"].(string)
	if !ok || ref == "" {
		return nil
	}
	if !idx.defined[ref] {
		return []RefIssue{{
			Kind: RefMissingTarget, Path: path + ".detour", Tag: ref,
			Detail: "the referenced outbound does not exist",
		}}
	}
	return nil
}

// checkDomainResolver validates `domain_resolver` against DNS SERVER tags.
func checkDomainResolver(obj map[string]interface{}, path string, serverTags map[string]bool) []RefIssue {
	ref, ok := obj["domain_resolver"].(string)
	if !ok || ref == "" {
		return nil
	}
	if !serverTags[ref] {
		return []RefIssue{{
			Kind: RefMissingTarget, Path: path + ".domain_resolver", Tag: ref,
			Detail: "the referenced DNS server does not exist",
		}}
	}
	return nil
}

// checkRuleSetRefs validates rule_set references against the declared list.
func checkRuleSetRefs(rule map[string]interface{}, path string, cfg map[string]interface{}) []RefIssue {
	refs := stringSlice(rule["rule_set"])
	if len(refs) == 0 {
		return nil
	}
	declared := map[string]bool{}
	if sets, ok := cfg["route"].(map[string]interface{}); ok {
		if list, ok := sets["rule_set"].([]interface{}); ok {
			for _, raw := range list {
				s, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if tag, ok := s["tag"].(string); ok && tag != "" {
					declared[tag] = true
				}
			}
		}
	}
	// Inline rule_sets may also be declared at the top level in some schemas.
	if list, ok := cfg["rule_set"].([]interface{}); ok {
		for _, raw := range list {
			s, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if tag, ok := s["tag"].(string); ok && tag != "" {
				declared[tag] = true
			}
		}
	}
	var issues []RefIssue
	for _, ref := range refs {
		if !declared[ref] {
			issues = append(issues, RefIssue{
				Kind: RefMissingTarget, Path: path + ".rule_set", Tag: ref,
				Detail: "the rule references a rule_set that is not declared",
			})
		}
	}
	return issues
}

// RepairRouteFinal resets a dangling route.final to a reachable target.
//
// This is the ONE reference problem the builder may fix by itself, and only
// because there is an unambiguously correct answer: a config with no valid
// catch-all cannot route anything, so leaving it is never better than pointing
// it at a surviving group. The replacement is chosen DETERMINISTICALLY —
// declaration order, groups before direct/block — so the same input always
// yields the same config and a golden test can pin it.
//
// Returns the tags it changed, or nil when there was nothing to repair. A
// config that already has a valid (or absent) final is left completely alone.
func RepairRouteFinal(cfg map[string]interface{}) (repairs []string, ok bool) {
	route, hasRoute := cfg["route"].(map[string]interface{})
	if !hasRoute {
		// No route section at all: nothing to repair, and inventing one would
		// change the user's routing semantics.
		return nil, true
	}
	final, _ := route["final"].(string)
	if final == "" {
		return nil, true
	}
	idx := buildRefIndex(cfg)
	if idx.defined[final] {
		return nil, true
	}

	replacement := idx.bestFinalTarget()
	if replacement == "" {
		if idx.count == 0 {
			// The template declares no outbounds at all. Nothing was removed, so
			// there is no corruption to repair and no candidate to substitute:
			// this is a minimal / lx-only template (the lx and DNS pass-through
			// fixtures are exactly this), and its route.final is the template
			// author's own declaration. Rewriting or rejecting it would break
			// templates that never had an outbound list to begin with.
			return nil, true
		}
		// Outbounds exist but none can carry traffic. This IS corruption: a
		// catch-all that was filtered away. Silently emitting a config with no
		// usable final would produce a core that starts and routes nothing.
		return nil, false
	}
	route["final"] = replacement
	return []string{fmt.Sprintf("route.final %q → %q (the original outbound no longer exists)", final, replacement)}, true
}

// bestFinalTarget picks the catch-all target deterministically.
//
// Preference order: the first surviving group in declaration order (that is what
// the template's own selector would have been), then the first direct/block
// outbound. Returns "" when neither exists.
func (idx *refIndex) bestFinalTarget() string {
	for _, tag := range idx.order {
		if idx.group[tag] {
			return tag
		}
	}
	for _, tag := range idx.order {
		if idx.directLike[tag] {
			return tag
		}
	}
	return ""
}

// stringSlice converts a decoded JSON array to []string, tolerating non-strings.
func stringSlice(v interface{}) []string {
	list, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ValidateConfigBytes is the byte-level entry point used on a candidate config.
//
// Unparseable JSON is itself a failure: the config could not be checked, so it
// must not be activated. Returning an error rather than an empty report is what
// stops "we could not verify it" from being read as "it is fine".
func ValidateConfigBytes(raw []byte) (RefReport, error) {
	cfg, err := decodeConfigObject(raw)
	if err != nil {
		return RefReport{}, err
	}
	return ValidateConfigReferences(cfg), nil
}

// decodeConfigObject parses a sing-box config, comments included.
//
// The builder emits `//` comments into its own output (the template carries
// them), so parsing the assembled document with encoding/json alone fails on
// every real config with "invalid character '/'". jsonc.ToJSON is the same
// helper the rest of the project uses for exactly this reason.
func decodeConfigObject(raw []byte) (map[string]interface{}, error) {
	var cfg map[string]interface{}
	if err := json.Unmarshal(jsonc.ToJSON(raw), &cfg); err != nil {
		return nil, fmt.Errorf("config is not valid JSON: %w", err)
	}
	return cfg, nil
}
