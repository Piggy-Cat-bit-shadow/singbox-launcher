package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"singbox-launcher/backend/service"
	"singbox-launcher/core/build"
	"singbox-launcher/core/config/configtypes"
	"singbox-launcher/core/state"
	"singbox-launcher/core/template"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"
)

// runCheckConfig is the config-pipeline smoke test behind `-check-config`.
//
// # WHY THIS EXISTS
//
// The macOS Action built a correct app that could not start a core. Check, Build
// and Artifact acceptance all passed, because every one of them asked "did the
// binary build?" and none asked "does the configuration this app produces
// actually work?". The defect that reached a user was a generated config whose
// `route.final` named an outbound that did not exist, and no amount of building
// could have revealed it.
//
// So this runs the same pipeline a Start runs, minus the launch:
//
//	load the bundled template
//	  → build config.json from the resolved layout
//	  → validate every internal reference
//	  → (when a core is given) sing-box check -c <config>
//
// It deliberately uses the REAL template from the resolved layout rather than a
// fixture, because the failure mode being guarded against is "the shipped
// template and the code disagree" — a fixture would agree with the code by
// construction and could never catch it.
//
// corePath is optional. Internal reference validation always runs; the external
// `sing-box check` runs when a core is supplied, because CI may not have one and
// a gate that silently skips is not a gate. Absence is REPORTED, not assumed
// away.
func runCheckConfig(layout paths.Layout, corePath string, out io.Writer) error {
	templatePath := filepath.Join(string(layout.Data), "bin", "wizard_template.json")
	if _, err := os.Stat(templatePath); err != nil {
		// The bundled template is the product's own file. If it is not where the
		// layout says, the app cannot generate a config at all, and saying that
		// plainly is more useful than a downstream parse error.
		return fmt.Errorf("bundled template not found at %s: %w", templatePath, err)
	}
	fmt.Fprintf(out, "template: %s\n", templatePath)

	// A fresh install has no state.json, and the app treats that as "config.json
	// is not ours to rebuild" rather than as an error. CI has no install either,
	// so without a state file the real build path would be SKIPPED and this smoke
	// test would prove nothing about it.
	//
	// Seeding the same minimal state a first run produces is what makes the test
	// exercise the pipeline instead of the skip.
	if err := ensureMinimalState(layout, out); err != nil {
		return err
	}

	backend, err := service.New(layout)
	if err != nil {
		return fmt.Errorf("initialise backend: %w", err)
	}
	defer backend.Shutdown()

	// Build the config exactly as a start would. RebuildConfigIfDirty is the same
	// call the pre-start hook makes, so this is the real pipeline and not a
	// parallel one that could drift from it.
	if err := backend.RebuildConfigForCheck(); err != nil {
		return fmt.Errorf("build config from template: %w", err)
	}

	configPath := backend.ConfigPath()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read generated config %s: %w", configPath, err)
	}
	fmt.Fprintf(out, "config:   %s (%d bytes)\n", configPath, len(raw))

	// 1. INTERNAL reference validation — always.
	report, err := build.ValidateConfigBytes(raw)
	if err != nil {
		return fmt.Errorf("generated config is not parseable: %w", err)
	}
	if !report.OK() {
		return fmt.Errorf("generated config has dangling references:\n%s", report.Error())
	}
	fmt.Fprintf(out, "references: OK\n")

	// 2. The route.final sanity check, stated explicitly.
	//
	// This is the invariant the incident violated, and asserting it by name means
	// the failure message names the incident rather than reporting a generic
	// dangling reference.
	if err := assertRouteFinalResolves(raw); err != nil {
		return err
	}
	fmt.Fprintf(out, "route.final: resolves\n")

	// 3. External `sing-box check`, when a core is available.
	if corePath == "" {
		fmt.Fprintf(out, "sing-box check: SKIPPED (no -core given)\n")
		return nil
	}
	if _, err := os.Stat(corePath); err != nil {
		return fmt.Errorf("-core %s is not usable: %w", corePath, err)
	}
	cmd := exec.Command(corePath, "check", "-c", configPath)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sing-box check REJECTED the generated config: %w\n%s",
			err, strings.TrimSpace(string(combined)))
	}
	fmt.Fprintf(out, "sing-box check: accepted\n")
	return nil
}

// assertRouteFinalResolves checks the one invariant the incident broke, by name.
//
// It duplicates a case ValidateConfigReferences already covers, and that is
// deliberate: this function exists so the FAILURE MESSAGE names route.final and
// lists the alternatives. A smoke test whose output is "some reference is
// dangling" sends the reader back to the code; one that prints the missing tag
// and what exists sends them to the fix.
func assertRouteFinalResolves(raw []byte) error {
	cfg, err := build.DecodeConfigForReport(raw)
	if err != nil {
		return fmt.Errorf("cannot parse the generated config: %w", err)
	}
	route, ok := cfg["route"].(map[string]interface{})
	if !ok {
		return nil // a template without a route section has no catch-all to check
	}
	final, _ := route["final"].(string)
	if final == "" {
		return nil
	}
	tags := build.OutboundTags(cfg)
	if len(tags) == 0 {
		// A config that names a catch-all but declares no outbounds at all cannot
		// route anything; the core would refuse it exactly as it refused the
		// incident's config.
		return fmt.Errorf("route.final is %q but the config declares NO outbounds", final)
	}
	for _, tag := range tags {
		if tag == final {
			return nil
		}
	}
	return fmt.Errorf("route.final references missing outbound %q.\nAvailable outbounds:\n- %s",
		final, strings.Join(tags, "\n- "))
}

// ensureMinimalState writes a default state.json when none exists, so the real
// build path runs rather than the "not ours to rebuild" skip.
//
// It never OVERWRITES an existing state: the point is to model a first run, and
// clobbering a real one would turn a smoke test into a destructive operation.
func ensureMinimalState(layout paths.Layout, out io.Writer) error {
	statePath := platform.GetWizardStatePath(layout.Data)
	if _, err := os.Stat(statePath); err == nil {
		fmt.Fprintf(out, "state:    %s (existing)\n", statePath)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect state file %s: %w", statePath, err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	st := state.New()
	// The template's own global outbounds belong in state.
	//
	// This is not test scaffolding: `parser_config.outbounds` are materialized
	// into the config ONLY when state.json lists them (SyncOutboundsWithTemplate
	// preserves Ref=#TEMPLATE# entries but never creates them), so a state
	// without them produces a config with no global selectors at all — and
	// `route.final`, which names one, then points at nothing. Seeding them is
	// what a first-run wizard save does.
	if err := seedTemplateOutbounds(st, layout); err != nil {
		return err
	}
	// A subscription with ONE node.
	//
	// Without a source the real pipeline refuses ("no enabled sources"), which is
	// correct behaviour but proves nothing about generation: the incident was a
	// config that WAS generated and pointed at an outbound that did not exist. A
	// node is what makes the required-selector path run at all.
	seedFixtureSubscription(st)
	if err := st.Save(statePath); err != nil {
		return fmt.Errorf("seed a minimal state.json at %s: %w", statePath, err)
	}
	fmt.Fprintf(out, "state:    %s (seeded with a one-node fixture subscription)\n", statePath)
	return nil
}

// seedFixtureSubscription adds one enabled subscription holding one node, built
// from a real share link so the node travels through the same parser path a
// fetched subscription does.
//
// The link is a locally-resolvable fixture (TEST-NET-1, RFC 5737) and is never
// dialled: the smoke test supplies the node directly, exactly as a refreshed
// subscription would, so CI needs no network and no live credentials.
func seedFixtureSubscription(st *state.State) {
	const link = "ss://YWVzLTEyOC1nY206cHc@192.0.2.10:8388#smoke-node"
	st.Sources = append(st.Sources, state.Source{
		Node: state.Node{
			Kind:    state.SourceKindSubscription,
			Enabled: true,
		},
		URL: "https://example.invalid/smoke-subscription",
		Nodes: []state.Node{{
			Kind:    state.SourceKindServer,
			Tag:     "smoke-node",
			Enabled: true,
			// Body is the node's PARSED sing-box outbound. A node without one is
			// not "materialized" and is skipped entirely, so the fixture must
			// carry the same field a refreshed subscription produces.
			Body:   json.RawMessage(`{"type":"shadowsocks","server":"192.0.2.10","server_port":8388,"method":"aes-128-gcm","password":"pw"}`),
			Origin: &state.Origin{Kind: state.OriginKindURI, Raw: link},
		}},
	})
}

// seedTemplateOutbounds mirrors the template's global outbounds into state, the
// way a wizard save does.
//
// A `Ref: #TEMPLATE#` entry is a THIN reference — tag plus ref, body resolved
// from the template at build time. Writing the tag alone would be a direct entry
// with no body, which the builder treats very differently, so the ref is set
// explicitly.
func seedTemplateOutbounds(st *state.State, layout paths.Layout) error {
	td, err := template.LoadTemplateData(layout)
	if err != nil {
		return fmt.Errorf("load template to seed outbounds: %w", err)
	}
	for _, ob := range td.GlobalOutbounds() {
		if ob.Tag == "" {
			continue
		}
		// State.Directions is the canonical, serialized home of the global
		// outbounds (JSON key "directions"). Writing ParserConfig.Outbounds
		// instead LOOKS right and silently does nothing: that field is not part
		// of state v8, so the entries vanish on Save and the config comes back
		// with no global selectors — which is the very failure this smoke test
		// exists to catch, and it caught it in my own seeding first.
		st.Directions = append(
			st.Directions,
			configtypes.Direction{
				Tag:          ob.Tag,
				Type:         ob.Type,
				Required:     ob.Required,
				AddOutbounds: ob.AddOutbounds,
				Filters:      ob.Filters,
				Options:      ob.Options,
				Ref:          configtypes.RefTemplate,
			},
		)
	}
	return nil
}
