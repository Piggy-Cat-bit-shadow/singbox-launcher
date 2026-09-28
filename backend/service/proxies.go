// Proxy service: the proxy surface the menu bar needs, projected from the
// existing core services.
//
// Nothing here re-implements proxy logic. Group discovery reads the selector
// outbounds from the active config, listing/switching/latency all go through
// the same services.ProxyTransport the Fyne Servers tab used, so classic and
// daemon engines keep behaving exactly as before and the backend stays a thin
// adapter rather than a second implementation.

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/core/config"
	coreservices "singbox-launcher/core/services"
	"singbox-launcher/internal/debuglog"
)

// delayNotMeasured is the wire value for "latency has never been measured".
//
// It is deliberately not 0: 0 ms is a legitimate reading, and conflating the
// two would show an untested node as instant.
const delayNotMeasured int64 = -1

// transport resolves the proxy transport for the current engine.
//
// An unavailable transport is an ordinary state, not an error: it just means
// the core is stopped or the Clash API is not configured yet. Callers turn it
// into Available=false so the UI explains it instead of showing a failure.
func (b *Backend) transport() (coreservices.ProxyTransport, bool) {
	if b.ac == nil || b.ac.APIService == nil {
		return nil, false
	}
	if t := b.ac.APIService.TransportOverride(); t != nil {
		return t, true
	}
	baseURL, token, enabled := b.ac.APIService.GetClashAPIConfig()
	if !enabled || baseURL == "" {
		return nil, false
	}
	return coreservices.NewClashTransport(baseURL, token), true
}

// ProxyGroups lists the switchable selector groups.
//
// The group set comes from the config, not from the Clash API, so the picker is
// still populated while the core is stopped; the live selection is added when
// a transport is available.
func (b *Backend) ProxyGroups() (protocol.ProxyList, error) {
	if b.ac == nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	names, defaultGroup, err := config.GetSelectorGroupsFromConfig(b.ac.FileService.ConfigPath)
	if err != nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "config_unreadable",
			Message:     "cannot read the selector groups from config.json: " + err.Error(),
			Recoverable: true,
		}
	}

	// The names above come from config.json. The Clash API below answers about whatever
	// config the RUNNING core loaded, and after a rebuild those are different documents.
	// Reporting that disagreement is the point: without it the picker lists groups the
	// live core does not have and every switch to one fails naming a group the user is
	// looking at.
	restartRequired := b.RuntimeConfigDiverged()

	transport, available := b.transport()
	groups := make([]protocol.ProxyGroup, 0, len(names))
	// Set once the engine reports it has no group capability at all. Checked
	// after the first group so a single capability probe is enough, and kept
	// for the whole loop so the remaining groups are not asked again.
	unsupported := false
	for _, name := range names {
		g := protocol.ProxyGroup{
			Name:        name,
			DisplayName: name,
		}
		if transport != nil && !unsupported {
			// A failure here is not fatal: the group is still switchable, we
			// just cannot report its current node yet — EXCEPT when the engine
			// cannot list proxies at all, which is a capability answer that
			// belongs in the response rather than in a log line.
			proxies, now, gerr := transport.GroupProxies(name)
			switch {
			case gerr == nil:
				g.Count = len(proxies)
				g.Selected = now
				g.SelectedDisplay = now
				if g.Type == "" {
					g.Type = groupTypeOf(proxies, now)
				}
			case errors.Is(gerr, coreservices.ErrProxyListUnsupported):
				unsupported = true
				debuglog.InfoLog("proxy groups: the active engine cannot list proxies")
			default:
				debuglog.DebugLog("proxy groups: cannot read group %q: %v", name, gerr)
			}
		}
		groups = append(groups, g)
	}

	// An engine that cannot list proxies reports no live counts. The group
	// NAMES still come from the config, so the picker is populated and the user
	// can see what the config defines — they simply cannot be probed here.
	available = available && !unsupported

	return protocol.ProxyList{
		Groups:                 groups,
		Proxies:                []protocol.Proxy{},
		Group:                  defaultGroup,
		Available:              available,
		Supported:              protocol.BoolPtr(!unsupported),
		UnsupportedReason:      unsupportedReason(unsupported),
		RuntimeRestartRequired: restartRequired,
	}, nil
}

// unsupportedReason names why the engine cannot list proxies, or "" when it can.
//
// A machine-readable token rather than prose: the frontend owns the wording, so
// the backend does not have to ship translated strings for a UI concern.
func unsupportedReason(unsupported bool) string {
	if !unsupported {
		return ""
	}
	return "daemon_no_group_rpc"
}

// groupTypeOf reports the Clash type of the selected node, falling back to the
// first proxy. Selector groups report their own type only inconsistently, so
// this is a best-effort label rather than a contract.
func groupTypeOf(proxies []api.ProxyInfo, selected string) string {
	for _, p := range proxies {
		if selected != "" && p.Name == selected {
			return p.ClashType
		}
	}
	if len(proxies) > 0 {
		return proxies[0].ClashType
	}
	return ""
}

// Proxies lists the nodes of one group, marking the current selection.
func (b *Backend) Proxies(group string) (protocol.ProxyList, error) {
	if b.ac == nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	// An empty group means "whatever the config selects by default", so a
	// frontend that only knows it wants a proxy list does not have to
	// discover the group name first.
	if strings.TrimSpace(group) == "" {
		names, defaultGroup, err := config.GetSelectorGroupsFromConfig(b.ac.FileService.ConfigPath)
		if err != nil {
			return protocol.ProxyList{}, &protocol.Error{
				Code:        "config_unreadable",
				Message:     "cannot read the selector groups from config.json: " + err.Error(),
				Recoverable: true,
			}
		}
		group = defaultGroup
		if group == "" && len(names) > 0 {
			group = names[0]
		}
		if group == "" {
			return protocol.ProxyList{
				Groups:    []protocol.ProxyGroup{},
				Proxies:   []protocol.Proxy{},
				Available: false,
				Supported: protocol.BoolPtr(true),
			}, nil
		}
	}

	transport, ok := b.transport()
	if !ok {
		// The group is known but there is no way to read it yet. This is the
		// normal "core is stopped" case, so it is reported as unavailable
		// rather than as an error the user cannot act on.
		// Supported but not up yet: "start the core" is the next step, which is
		// a different screen from "this engine cannot do it".
		return protocol.ProxyList{
			Groups:    []protocol.ProxyGroup{},
			Proxies:   []protocol.Proxy{},
			Group:     group,
			Available: false,
			Supported: protocol.BoolPtr(true),
		}, nil
	}

	infos, selected, err := transport.GroupProxies(group)
	if err != nil {
		// The engine has no group capability. Reported as a capability, not as
		// a failure: an error here produced a red banner naming an internal RPC
		// the user has no way to act on.
		if errors.Is(err, coreservices.ErrProxyListUnsupported) {
			return protocol.ProxyList{
				Groups:            []protocol.ProxyGroup{},
				Proxies:           []protocol.Proxy{},
				Group:             group,
				Available:         false,
				Supported:         protocol.BoolPtr(false),
				UnsupportedReason: unsupportedReason(true),
			}, nil
		}
		if errors.Is(err, api.ErrPlatformInterrupt) {
			return protocol.ProxyList{}, &protocol.Error{
				Code:        "interrupted",
				Message:     "the system went to sleep while reading the proxy list",
				Recoverable: true,
			}
		}
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "proxy_list_failed",
			Message:     fmt.Sprintf("cannot read the proxies of group %q: %v", group, err),
			Recoverable: true,
		}
	}

	proxies := make([]protocol.Proxy, 0, len(infos))
	for _, p := range infos {
		delay := p.Delay
		if delay <= 0 {
			delay = delayNotMeasured
		}
		status := ""
		lastError := b.ac.APIService.GetLastPingError(p.Name)

		// OVERLAY. A result this session measured is more authoritative than
		// whatever the engine reports, because the engine's group snapshot is
		// not a reliable echo of a test that just ran: a daemon's
		// URLTestOutbound reply says nothing about the next GetGroups. Without
		// this, a freshly measured 42 ms would be replaced by -1 on re-read and
		// the row would flicker back to "—" the moment anything refreshed.
		if m, ok := b.ac.APIService.GetMeasurement(p.Name); ok {
			status = string(m.Status)
			switch m.Status {
			case coreservices.MeasurementSuccess:
				delay = m.Delay
			case coreservices.MeasurementTimeout, coreservices.MeasurementFailed,
				coreservices.MeasurementUnsupported, coreservices.MeasurementCancelled:
				// The node was measured and did NOT answer. Keeping the old
				// number would present a stale value as present health, so the
				// engine's own value is discarded too: a node we just failed to
				// reach is not "42 ms" because some core cache says so.
				delay = delayNotMeasured
			}
			if m.Error != "" {
				lastError = m.Error
			}
		}

		proxies = append(proxies, protocol.Proxy{
			Name:        p.Name,
			DisplayName: p.DisplayOrName(),
			Type:        p.ClashType,
			Delay:       delay,
			Group:       group,
			Selected:    p.Name == selected,
			LastError:   lastError,
			Status:      status,
		})
	}

	// Reaching here means the engine answered with nodes, so it demonstrably
	// supports this. Stated explicitly rather than left absent: the frontend
	// reads an absent value as "assume supported", which is the right default
	// for an older backend but should not be how the CURRENT backend answers.
	return protocol.ProxyList{
		Groups:       []protocol.ProxyGroup{},
		Proxies:      proxies,
		Group:        group,
		Available:    true,
		Supported:    protocol.BoolPtr(true),
		Capabilities: b.capabilitiesDTO(),
	}, nil
}

// capabilitiesDTO projects the engine's per-action abilities for the wire.
func (b *Backend) capabilitiesDTO() *protocol.ProxyActionCapabilities {
	c := b.proxyActionCapabilities()
	return &protocol.ProxyActionCapabilities{
		CanList: c.CanList, CanSwitch: c.CanSwitch,
		CanTestSingle: c.CanTestSingle, CanTestGroup: c.CanTestGroup,
		ListReason: c.ListReason, SwitchReason: c.SwitchReason, TestReason: c.TestReason,
	}
}

// SwitchProxy selects a node inside a group and reports the new state.
//
// The switch goes through the same call the old UI used, so the side effects
// (active name, last-selected memory) are preserved. The returned list is
// re-read from the core, because the core is what actually decides whether the
// switch took effect.
func (b *Backend) SwitchProxy(group, name string) (protocol.ProxyList, error) {
	if b.ac == nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	if strings.TrimSpace(name) == "" {
		return protocol.ProxyList{}, &protocol.Error{
			Code: "bad_request", Message: "proxy name is required", Recoverable: false,
		}
	}

	transport, ok := b.transport()
	if !ok {
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "core_not_running",
			Message:     "start the core before switching proxies",
			Recoverable: true,
		}
	}

	if err := b.ac.APIService.SwitchProxyVia(transport, group, name); err != nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "switch_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: proxy %q switched to %q in group %q", group, name, group)
	b.emit(protocol.EventProxySelectionChanged, map[string]any{"group": group, "name": name})

	// Re-read so the client renders what the core reports, not what it asked
	// for. A switch that silently did not take would otherwise look applied.
	return b.Proxies(group)
}

// TestProxy measures one node's latency and returns the refreshed list.
func (b *Backend) TestProxy(group, name string) (protocol.ProxyList, error) {
	if b.ac == nil {
		return protocol.ProxyList{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	caps := b.proxyActionCapabilities()
	if !caps.CanTestSingle {
		return protocol.ProxyList{}, capabilityErrorFor(caps)
	}
	transport, ok := b.transport()
	if !ok {
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "core_not_running",
			Message:     "start the core before testing latency",
			Recoverable: true,
		}
	}

	// One measurement primitive for single and group tests, so timeout handling
	// and error classification cannot drift between them.
	outcome := measureProxy(context.Background(), transport, proxyNode{Group: group, Name: name})

	// STORE the result. The previous version measured, discarded the number and
	// re-read the list hoping the core would echo it back — which Classic
	// sometimes did and daemon did not, since a URLTestOutbound reply is not a
	// promise about the next group snapshot.
	b.ac.APIService.SetMeasurement(name, coreservices.ProxyMeasurementState{
		Delay:      outcome.Delay,
		Status:     outcome.Status,
		Error:      outcome.Error,
		MeasuredAt: outcome.MeasuredAt,
		Generation: b.groupTests.ActiveRunID(),
	})
	if outcome.Status != coreservices.MeasurementSuccess {
		debuglog.DebugLog("backend: latency test for %q: %s (%s)",
			name, outcome.Status, outcome.Error)
	}

	// The returned list is overlaid with the measurement just taken, so the row
	// shows the value this request produced.
	return b.Proxies(group)
}

// TestProxyGroup measures every node in a group.
//
// Superseded by RunGroupTest, which measures with bounded concurrency and emits
// progress. Kept for API stability; it forwards so there is exactly ONE
// scheduler rather than a second serial one that could silently diverge.
func (b *Backend) TestProxyGroup(group string) (protocol.ProxyList, error) {
	res, err := b.RunGroupTest(context.Background(), group)
	if err != nil {
		return protocol.ProxyList{}, err
	}
	return res.Proxies, nil
}

// proxyActionCapabilities reports what the active engine can do, per action.
//
// CLASSIC answers "all three" because it drives the core's Clash-compatible HTTP
// API, whose endpoints are part of the core's contract rather than of this
// launcher's generated client. Daemon mode asks the daemon, because the reachable
// daemon's method set is not knowable from the client stub — the drift that
// produced the whole compatibility layer.
//
// This is the ONLY place the engine's abilities are decided. The frontend reads
// these booleans and must never branch on the backend mode, so a future engine
// needs no UI change.
func (b *Backend) proxyActionCapabilities() core.ProxyActionCapabilities {
	if b.ac == nil {
		return core.ProxyActionCapabilities{
			ListReason: core.ReasonUnknown, SwitchReason: core.ReasonUnknown, TestReason: core.ReasonUnknown,
		}
	}
	if db, ok := b.ac.Backend().(*core.DaemonBackend); ok {
		return db.ProxyActionCapabilities()
	}
	return core.ProxyActionCapabilities{
		CanList: true, CanSwitch: true, CanTestSingle: true, CanTestGroup: true,
	}
}

// ProxyActionCapabilities exposes the per-action set to the protocol layer.
func (b *Backend) ProxyActionCapabilities() core.ProxyActionCapabilities {
	return b.proxyActionCapabilities()
}

// capabilityErrorFor turns an unsupported action into a typed protocol error.
//
// The code is stable and machine-readable so the UI can explain the situation in
// the user's language; the message is a diagnostic fallback, never the primary
// text a user reads.
func capabilityErrorFor(caps core.ProxyActionCapabilities) error {
	reason := caps.ListReason
	code := "proxy_list_unsupported"
	msg := "the active engine does not support listing proxies"

	switch {
	case !caps.CanTestGroup && caps.CanList:
		// The specific case worth calling out: nodes are visible, but latency
		// cannot be measured. Reporting this as a list failure would disable a
		// screen that works.
		code = "proxy_latency_unsupported"
		msg = "the active engine does not support measuring proxy latency"
		reason = caps.TestReason
	case !caps.CanSwitch && caps.CanList:
		code = "proxy_switch_unsupported"
		msg = "the active engine does not support switching proxies"
		reason = caps.SwitchReason
	}
	return &protocol.Error{Code: code, Message: msg, Recoverable: false, Reason: reason}
}
