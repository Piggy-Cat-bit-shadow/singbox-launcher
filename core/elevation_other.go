//go:build !windows || 386

// File elevation_other.go — non-Windows stub for the elevation flow.
//
// Elevation (UAC, restart-as-administrator, the TUN rights gate) is a Windows
// mechanism. The menu-bar product is macOS-only, so on other platforms the
// gate is simply closed and nothing is elevated.
package core

import "singbox-launcher/core/state"

// ElevateAtStartSupported reports whether the platform auto-elevates at start.
// Only Windows does.
const ElevateAtStartSupported = false

// AutostartFlagName is the installer flag for launch-at-login. Implemented on
// Windows only; declared here so the shared flag plumbing compiles.
const AutostartFlagName = "autostart"

// windowsNotElevated is always false off Windows: there is nothing to elevate.
func windowsNotElevated() bool { return false }

// logSkippedAdminCleanups is a no-op off Windows.
func logSkippedAdminCleanups() {}

// tunNeedsElevation is always false off Windows; TUN does not need a UAC gate.
func (ac *AppController) tunNeedsElevation() bool { return false }

// showTunElevationDialog has nothing to show off Windows.
func (ac *AppController) showTunElevationDialog() {}

// SwitchToProxyMode is not a mode switch off Windows.
func (ac *AppController) SwitchToProxyMode() error { return nil }

// KillNeedsElevation is always false off Windows.
func (ac *AppController) KillNeedsElevation(error) bool { return false }

// ShowKillNeedsElevation has nothing to show off Windows.
func (ac *AppController) ShowKillNeedsElevation() {}

// proxyInListenPort reports the mixed inbound port, used by the Windows
// elevation copy. Off Windows it is only referenced by shared text.
func (ac *AppController) proxyInListenPort() string { return "" }

// setLocalStateVars is unused off Windows.
func setLocalStateVars(*AppController, []state.SettingVar) error { return nil }
