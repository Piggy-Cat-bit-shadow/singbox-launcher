// Config maintenance: rebuilding config.json from state and refreshing
// subscriptions.
//
// Both delegate to the existing core services. The only thing added here is a
// result DTO, because these operations are the ones a menu-bar user runs "blind"
// — they need to know how many sources succeeded, not merely that no error was
// returned. A refresh where every source failed is a failure the user must see,
// even though the call itself returned nil.

package service

import (
	"os"
	"strconv"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// MaintenanceResult reports what a rebuild or refresh actually did.
type MaintenanceResult struct {
	// OK is false when the operation completed but achieved nothing useful
	// (for example every subscription source failed).
	OK bool `json:"ok"`
	// Message is a short human-readable summary, already localised by the
	// core where the core produced it.
	Message string `json:"message"`
	// TotalSources, SucceededSources and FailedSources describe the refresh.
	// They stay zero for a plain rebuild, which has no per-source notion.
	TotalSources     int `json:"total_sources"`
	SucceededSources int `json:"succeeded_sources"`
	FailedSources    int `json:"failed_sources"`
	// NodesCount is how many nodes the rebuild produced.
	NodesCount int `json:"nodes_count"`
	// CoreSkips lists nodes the core cannot run, so the user learns why a
	// subscription lost entries instead of silently getting fewer nodes.
	CoreSkips []string `json:"core_skips"`
}

// ReloadConfig forces a full rebuild of config.json from the current state.
//
// Forced on purpose: the menu-bar action means "rebuild now", so it must not
// silently no-op because the dirty markers happen to be clean. That no-op
// behaviour is what the automatic background path wants, not what a button
// labelled "Reload Config" wants.
func (b *Backend) ReloadConfig() (MaintenanceResult, error) {
	if b.ac == nil {
		return MaintenanceResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: reload_config requested")

	// A rebuild replays the wizard state into config.json, so a config that was
	// not produced by the wizard cannot be rebuilt. Saying so plainly is the
	// difference between a dead end and a next step: the raw error names
	// state.json, which the user has never heard of, while this tells them the
	// configuration was made elsewhere and where to change it.
	if !b.configIsRebuildable() {
		return MaintenanceResult{}, &protocol.Error{
			Code: "not_rebuildable",
			Message: "This configuration was not created by JiejieBox's wizard, so it " +
				"cannot be rebuilt here. Edit config.json directly, or rebuild it " +
				"from the wizard on the desktop build.",
			Recoverable: false,
		}
	}

	if err := b.ac.RebuildConfigIfDirty(true); err != nil {
		return MaintenanceResult{}, &protocol.Error{
			Code:        "rebuild_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	// The core's view of the config may have changed (existence, node count),
	// so republish the state alongside the result.
	b.EmitCoreState()
	b.emit(protocol.EventProxiesChanged, map[string]any{"reason": "config_reloaded"})

	return MaintenanceResult{
		OK:      true,
		Message: "Configuration rebuilt from the current state.",
	}, nil
}

// configIsRebuildable reports whether a rebuild has the state it needs.
//
// Rebuilding is a replay of the wizard state, so without that state there is
// nothing to replay — the config on disk is then the only source of truth and
// must not be overwritten from an empty one.
func (b *Backend) configIsRebuildable() bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}
	statePath := platform.GetWizardStatePath(b.ac.FileService.Layout.Data)
	_, err := os.Stat(statePath)
	return err == nil
}

// UpdateSubscriptions refreshes every enabled subscription source.
func (b *Backend) UpdateSubscriptions() (MaintenanceResult, error) {
	if b.ac == nil {
		return MaintenanceResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	if b.ac.ConfigService == nil {
		return MaintenanceResult{}, &protocol.Error{
			Code:        "not_ready",
			Message:     "the configuration service is not available",
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: update_subscriptions requested")
	res, err := b.ac.ConfigService.UpdateConfigFromSubscriptions()
	if err != nil {
		return MaintenanceResult{}, &protocol.Error{
			Code:        "update_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	out := MaintenanceResult{OK: true}
	if res != nil {
		out.TotalSources = res.TotalSources
		out.SucceededSources = res.SucceededSources
		out.FailedSources = res.FailedSources
		out.NodesCount = res.NodesCount
		for _, skip := range res.CoreSkips {
			// CoreSkip.Summary() is the same sentence the core logs, so the
			// menu bar and the backend log never disagree about why nodes
			// were dropped.
			out.CoreSkips = append(out.CoreSkips, skip.Summary())
		}

		// A refresh where every source failed returns no error but changed
		// nothing. Reporting OK here would tell the user their subscription
		// is up to date when it is not.
		if res.TotalSources > 0 && res.SucceededSources == 0 {
			out.OK = false
			out.Message = "Every subscription source failed; nothing was updated."
		} else {
			out.Message = summaryMessage(res.TotalSources, res.SucceededSources, res.FailedSources, res.NodesCount)
		}
	} else {
		out.Message = "Subscriptions updated."
	}

	b.EmitCoreState()
	b.emit(protocol.EventProxiesChanged, map[string]any{"reason": "subscriptions_updated"})
	return out, nil
}

// summaryMessage builds the one-line result the UI shows.
func summaryMessage(total, ok, failed, nodes int) string {
	if total == 0 {
		return "No subscription sources are enabled."
	}
	if failed == 0 {
		return plural(nodes, "node") + " from " + plural(total, "source") + "."
	}
	return plural(nodes, "node") + " from " + plural(ok, "source") +
		"; " + plural(failed, "source") + " failed."
}

// plural renders "1 node" / "3 nodes" without pulling in a format package for
// one string.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
