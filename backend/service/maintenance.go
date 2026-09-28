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
	"strconv"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/internal/debuglog"
)

// MaintenanceResult reports what a rebuild or refresh actually did.
type MaintenanceResult struct {
	// OK is false when the operation completed but achieved nothing useful, or
	// when a phase it depends on failed.
	//
	// It is the OVERALL verdict, derived from the per-phase fields below rather than
	// assumed from the first phase's success. Reporting OK because the refresh worked
	// told the user "N nodes from M sources" while config.json had not been updated
	// at all and the core kept running the old one.
	OK bool `json:"ok"`
	// RefreshOK reports the fetch phase: did the sources refresh succeed.
	RefreshOK bool `json:"refresh_ok"`
	// RebuildOK reports the materialisation phase: is config.json now current.
	//
	// SEPARATE FROM RefreshOK because the phases fail independently, and the
	// difference is exactly what the user needs: a refresh that worked with a rebuild
	// that failed means the node list is updated in state but the running config is
	// still the old one. Collapsing them into one flag loses that.
	RebuildOK bool `json:"rebuild_ok"`
	// RebuildError carries why the rebuild failed, when it did.
	//
	// The core used to log this and return nil, so the failure existed only in a log
	// file the user never sees.
	RebuildError string `json:"rebuild_error,omitempty"`
	// ConfigStale reports whether config.json still lags the state after this
	// operation. True means the figures above describe state that is not yet built.
	ConfigStale bool `json:"config_stale"`
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
		// The message depends on WHICH refusal this is, because the two have
		// different remedies. UNKNOWN must not be described as "made by another
		// tool" — that is an unproven claim, and for a config written by an
		// older JiejieBox it is simply false.
		msg := "JiejieBox cannot confirm that it created this configuration, so it " +
			"will not rebuild it here. You can let JiejieBox manage it, or edit " +
			"config.json directly."
		if b.configOwnership() == OwnershipExternal {
			msg = "This configuration is managed by another tool, so it cannot be " +
				"rebuilt here. Edit config.json directly, or change it in the tool " +
				"that owns it."
		}
		return MaintenanceResult{}, &protocol.Error{
			Code:        "not_rebuildable",
			Message:     msg,
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

	// The build succeeded, so the launcher now owns the config on disk. Record
	// that before returning: the marker is what makes a LATER reload possible,
	// and it is written only here, after a real build.
	if err := b.markConfigManaged(); err != nil {
		debuglog.WarnLog("backend: reload succeeded but provenance could not be recorded: %v", err)
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

	// TWO PHASES, TWO OUTCOMES.
	//
	// The refresh and the rebuild fail independently, and a caller that learns only
	// "the operation returned no error" cannot tell the user anything true. A
	// successful refresh with a failed rebuild leaves the node list updated in state
	// while the RUNNING config is still the old one — the most misleading outcome
	// available, because everything the user can see says it worked.
	out := MaintenanceResult{
		RefreshOK: true,
		RebuildOK: res == nil || res.RebuildErr == nil,
	}
	if res != nil && res.RebuildErr != nil {
		out.RebuildError = res.RebuildErr.Error()
	}
	// The overall verdict requires BOTH phases. Anything less reports success for an
	// operation whose point — the config the core runs — did not happen.
	out.OK = out.RefreshOK && out.RebuildOK
	if b.ac.StateService != nil {
		out.ConfigStale = b.ac.StateService.IsConfigStale()
	}
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

// AdoptConfig hands ownership of config.json to JiejieBox.
//
// Called ONLY from an explicit user confirmation that states the consequence —
// that the launcher may from then on rebuild and overwrite the file. It is
// deliberately not reachable from any automatic path: seizing ownership of a
// file the launcher may not have written is exactly the kind of silent action
// that destroyed user configs in the first place.
//
// Restricted to UNKNOWN. A config that is already managed needs no adoption, and
// one that is external carries positive evidence of another owner that a button
// press does not overrule.
func (b *Backend) AdoptConfig() (MaintenanceResult, error) {
	if b.ac == nil {
		return MaintenanceResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	ownership := b.configOwnership()
	if ownership == OwnershipManaged {
		return MaintenanceResult{Message: "already managed"}, nil
	}
	if ownership == OwnershipExternal {
		return MaintenanceResult{}, &protocol.Error{
			Code: "not_adoptable",
			Message: "This configuration is managed by another tool. " +
				"JiejieBox will not take it over.",
			Recoverable: false,
		}
	}
	if !b.configExists() {
		return MaintenanceResult{}, &protocol.Error{
			Code: "no_config", Message: "there is no config.json to adopt", Recoverable: true,
		}
	}
	debuglog.InfoLog("backend: adopt_config requested — the user accepted the overwrite consequence")
	// The user has said the file is ours to manage, so the marker may be written
	// directly. No build is run: the point of adoption is to take responsibility
	// for the EXISTING file, not to replace it.
	if err := b.markConfigManaged(); err != nil {
		return MaintenanceResult{}, &protocol.Error{
			Code: "adopt_failed", Message: err.Error(), Recoverable: true,
		}
	}
	b.EmitCoreState()
	return MaintenanceResult{Message: "adopted"}, nil
}
