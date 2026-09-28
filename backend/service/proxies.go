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
	"errors"
	"fmt"
	"strings"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
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
		Groups:            groups,
		Proxies:           []protocol.Proxy{},
		Group:             defaultGroup,
		Available:         available,
		Supported:         protocol.BoolPtr(!unsupported),
		UnsupportedReason: unsupportedReason(unsupported),
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
		proxies = append(proxies, protocol.Proxy{
			Name:        p.Name,
			DisplayName: p.DisplayOrName(),
			Type:        p.ClashType,
			Delay:       delay,
			Group:       group,
			Selected:    p.Name == selected,
			LastError:   b.ac.APIService.GetLastPingError(p.Name),
		})
	}

	// Reaching here means the engine answered with nodes, so it demonstrably
	// supports this. Stated explicitly rather than left absent: the frontend
	// reads an absent value as "assume supported", which is the right default
	// for an older backend but should not be how the CURRENT backend answers.
	return protocol.ProxyList{
		Groups:    []protocol.ProxyGroup{},
		Proxies:   proxies,
		Group:     group,
		Available: true,
		Supported: protocol.BoolPtr(true),
	}, nil
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
	transport, ok := b.transport()
	if !ok {
		return protocol.ProxyList{}, &protocol.Error{
			Code:        "core_not_running",
			Message:     "start the core before testing latency",
			Recoverable: true,
		}
	}

	delay, err := transport.Delay(name)
	if err != nil {
		// A failed measurement is data, not a fatal error: the node is
		// unreachable, and the UI shows that per row.
		b.ac.APIService.SetLastPingError(name, err.Error())
		debuglog.DebugLog("backend: latency test for %q failed: %v", name, err)
	} else {
		b.ac.APIService.SetLastPingError(name, "")
		_ = delay
	}

	return b.Proxies(group)
}

// TestProxyGroup measures every node in a group.
//
// Sequential on purpose: firing hundreds of simultaneous handshakes starves
// in-flight traffic, which is the same reasoning behind the existing
// auto-ping cap (SPEC 039 §1.3).
func (b *Backend) TestProxyGroup(group string) (protocol.ProxyList, error) {
	list, err := b.Proxies(group)
	if err != nil {
		return protocol.ProxyList{}, err
	}
	if !list.Available {
		return list, nil
	}

	transport, ok := b.transport()
	if !ok {
		return list, nil
	}

	for _, p := range list.Proxies {
		if _, derr := transport.Delay(p.Name); derr != nil {
			b.ac.APIService.SetLastPingError(p.Name, derr.Error())
		} else {
			b.ac.APIService.SetLastPingError(p.Name, "")
		}
	}

	// Re-read after measuring so the returned delays are the ones the core
	// recorded, not the ones we hoped for.
	return b.Proxies(group)
}
