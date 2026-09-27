package ui

import (
	"fyne.io/fyne/v2/container"

	"singbox-launcher/core"
	"singbox-launcher/ui/components"
)

// wizardOverlayEnabled — feature flag for the main-window click-redirect
// overlay. When true (legacy behavior pre-v0.9.8), an invisible overlay
// sits on top of the main window while the configurator is open and
// redirects every click to focus the configurator → main window becomes
// effectively read-only.
//
// Set to false so users can drive Update / Restart / Start / Stop and the
// pages in parallel with the configurator. Flip back to true if you need
// the legacy "wizard owns the foreground" UX without ripping out the
// implementation.
//
// Independent of the wizard's *internal* ChildWindowsOverlay
// (`presenter.UpdateChildOverlay`), which still uses `components.ClickRedirect`
// over its own tabs to keep child dialogs (Edit Outbound, View Source,
// rule dialog) on top within the wizard window.
const wizardOverlayEnabled = false

// InitWizardOverlay optionally wraps the app's root content in the click
// redirect overlay, and subscribes to UIService.OnStateChange so that overlay
// visibility follows wizard open/close state. Extracted to a separate file for
// modularity and testability.
//
// **The base is the app's current root content, never the legacy AppTabs.**
// Since SPEC 144 that root is the sidebar shell
// (`App.content` = Border(sidebar, contentHost), built in NewApp). This
// function must not substitute `app.tabs` for it: AppTabs is kept only as a
// compatibility object for its `OnSelected` handler and `updateClashAPITabState`,
// and is not part of the visual tree. Overwriting `App.content` with it here is
// exactly the regression that made a fully-implemented sidebar invisible — the
// window fell back to the old top tab strip.
//
// When `wizardOverlayEnabled` is false (current default) this function leaves
// `App.content` untouched and registers no OnStateChange hook, so clicks on the
// main window flow normally to their targets while the wizard is open.
func InitWizardOverlay(app *App, controller *core.AppController) {
	if app == nil || controller == nil {
		return
	}

	if !wizardOverlayEnabled {
		// Nothing to do: `app.content` is already the correct sidebar shell
		// (or whatever the app installed as its root). Returning without
		// touching it keeps input flowing to Update / Restart / page controls
		// even while the configurator is open.
		return
	}

	// Overlay goes on top of whatever the app already shows. The AppTabs
	// fallback exists only for a hypothetical App whose root was never set;
	// it must never take precedence over a real root.
	base := app.content
	if base == nil {
		base = app.tabs
	}

	overlay := components.NewClickRedirect(controller.UIService)
	app.overlay = overlay
	app.content = container.NewStack(base, overlay)

	// Subscribe to UIService.OnStateChange to keep overlay visibility in sync
	if controller.UIService != nil {
		origOnState := controller.UIService.OnStateChange
		controller.UIService.OnStateChange = func() {
			if origOnState != nil {
				origOnState()
			}
			// OnStateChange вызывается из wizard.go на UI-потоке — fyne.Do не нужен
			app.updateWizardOverlay()
		}
		// Set initial overlay visibility
		app.updateWizardOverlay()
	}
}

// updateWizardOverlay shows or hides the click redirect overlay depending on
// whether the Wizard is open. Kept here with InitWizardOverlay so all overlay
// logic lives in the same file.
func (a *App) updateWizardOverlay() {
	if a.overlay == nil || a.core == nil || a.core.UIService == nil {
		return
	}
	if a.core.UIService.WizardWindow != nil {
		a.overlay.Show()
		a.overlay.Refresh()
	} else {
		a.overlay.Hide()
		a.overlay.Refresh()
	}
}
