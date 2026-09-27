// File uiport_glue.go — core's thin glue over internal/uiport.
//
// The port interface lives in internal/uiport so core and the Fyne presentation
// package can both reference it without an import cycle. This file holds only
// the accessor and the setter core needs.
package core

import "singbox-launcher/internal/uiport"

// SetUIPort installs the GUI port. Called by the GUI layer at startup; never
// called by the headless backend.
func (ac *AppController) SetUIPort(p uiport.Port) {
	if ac == nil {
		return
	}
	ac.uiPort = p
}

// UIPortOrNil returns the active GUI port, or nil when running headless.
func (ac *AppController) UIPortOrNil() uiport.Port {
	if ac == nil {
		return nil
	}
	return ac.uiPort
}

// ui returns the GUI port, or a no-op implementation when running headless.
//
// Call sites can then invoke port methods unconditionally. Capability checks
// that need to *know* whether a GUI exists still test uiPort directly.
func (ac *AppController) ui() uiport.Port {
	if ac == nil || ac.uiPort == nil {
		return uiport.Headless{}
	}
	return ac.uiPort
}
