// Package icons — embedded SVG resources for tab icons that aren't in
// fyne/theme. We tried emoji-in-label first (⚙️ ⚡ etc.) — works for
// most emoji because they include U+FE0F variation selector which forces
// emoji presentation, but Fyne's default font has no glyph for some
// codepoints (notably ⚡ U+26A1 even with VS-16) → tab rendered with
// blank space. Real SVGs via theme.NewThemedResource render reliably and
// inherit the active text color (currentColor).
package icons

import (
	_ "embed"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

//go:embed bolt.svg
var boltSVG []byte

// Bolt — lightning-bolt icon, used as the Core tab indicator. Replaces
// the ⚡ emoji that Fyne's default font couldn't render.
var Bolt fyne.Resource = &fyne.StaticResource{
	StaticName:    "bolt.svg",
	StaticContent: boltSVG,
}

//go:embed telegram.svg
var telegramSVG []byte

// Telegram — blue paper-plane, for Telegram support links (t.me / tg://).
// Kept as a StaticResource (NOT themed) so it stays Telegram-blue regardless
// of the active theme's foreground color.
var Telegram fyne.Resource = &fyne.StaticResource{
	StaticName:    "telegram.svg",
	StaticContent: telegramSVG,
}

//go:embed link.svg
var linkSVG []byte

// Link — generic chain/link icon for non-Telegram support / web-page links.
// Themed (currentColor) so it inherits the active text color.
var Link fyne.Resource = theme.NewThemedResource(&fyne.StaticResource{
	StaticName:    "link.svg",
	StaticContent: linkSVG,
})

// Navigation icons (SPEC 144).
//
// Why SVG instead of emoji. The old tab strip used emoji as navigation
// glyphs (🌐 Remote, ⚙️ Settings, 🔍 Diagnostics, ❓ Help). Emoji metrics
// come from whatever colour-emoji font the OS provides: baseline, optical
// size and advance width differ between macOS, Windows and Linux, and on a
// Linux box without an emoji font the glyph is simply missing (the same
// failure already documented above for ⚡). An SVG vector scales with the
// canvas, stays crisp at fractional scaling, and inherits the theme's text
// colour — so a nav item in dark mode is legible without a second asset.
//
// All of these are themed (currentColor): they follow the active theme and
// need no per-variant variants.
//
//go:embed nav_local.svg
var navLocalSVG []byte

//go:embed nav_remote.svg
var navRemoteSVG []byte

//go:embed nav_diagnostics.svg
var navDiagnosticsSVG []byte

//go:embed nav_settings.svg
var navSettingsSVG []byte

//go:embed nav_help.svg
var navHelpSVG []byte

//go:embed chevron_right.svg
var chevronRightSVG []byte

//go:embed chevron_down.svg
var chevronDownSVG []byte

func themed(name string, raw []byte) fyne.Resource {
	return theme.NewThemedResource(&fyne.StaticResource{StaticName: name, StaticContent: raw})
}

// NavLocal — local sing-box instance (map pin).
var NavLocal = themed("nav_local.svg", navLocalSVG)

// NavRemote — remote machines (grid).
var NavRemote = themed("nav_remote.svg", navRemoteSVG)

// NavDiagnostics — logs and maintenance (pulse).
var NavDiagnostics = themed("nav_diagnostics.svg", navDiagnosticsSVG)

// NavSettings — launcher preferences (gear).
var NavSettings = themed("nav_settings.svg", navSettingsSVG)

// NavHelp — about and links (question mark in a circle).
var NavHelp = themed("nav_help.svg", navHelpSVG)

// ChevronRight — collapsed parent row in the sidebar.
var ChevronRight = themed("chevron_right.svg", chevronRightSVG)

// ChevronDown — expanded parent row in the sidebar.
var ChevronDown = themed("chevron_down.svg", chevronDownSVG)
