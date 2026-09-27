// Typography — the one text scale for the whole app.
//
// Every label goes through a `Typography` role. The point is not abstraction for
// its own sake: before this existed, sizes were chosen per call site
// (`.caption2` here, `.callout` there), which is why the panel read as a pile of
// controls rather than a designed surface. A named role also makes the scale
// reviewable in one place instead of by grepping font calls.
//
// Fonts are SYSTEM fonts, always. No bundled family, no `Font.custom`, no
// per-language family switch:
//
//   * macOS picks the correct CJK glyphs for 简体中文 automatically, and does it
//     better than any family we could guess — PingFang SC for Chinese, SF Pro for
//     Latin, with matched optical sizes and weights.
//   * A bundled CJK font would add megabytes to the app and be one more thing to
//     license and update.
//   * System fonts keep SF Symbols aligned: a custom family changes the text
//     metrics around a symbol without changing the symbol, which is exactly how
//     icons end up looking "off by a pixel".
//
// So localization never touches typography. A string is longer or shorter, not
// a different typeface — the layout has to tolerate that instead (see the
// `lineLimit`/`fixedSize` guidance on the row primitives).

import SwiftUI

/// A text role. Sizes are fixed points, not `.font(.body)` text styles, because
/// the panel is a fixed-width menu bar surface rather than a document that
/// should rescale with the user's reading-size preference.
enum Typography {
    /// Screen title, in the panel header.
    static let pageTitle = Font.system(size: 15, weight: .semibold)
    /// Section label above a group of rows. Smaller and lighter than any row
    /// title on purpose: it is a label for the group, not a row, and making it
    /// the same weight as a row title is what made "Application" read as a
    /// clickable item.
    static let sectionHeader = Font.system(size: 11, weight: .semibold)
    /// Primary text of an interactive row.
    static let rowTitle = Font.system(size: 13, weight: .regular)
    /// Secondary line under a row title (status, error, help).
    static let rowSubtitle = Font.system(size: 11, weight: .regular)
    /// A row's trailing value (current selection, count, version).
    static let rowValue = Font.system(size: 12, weight: .regular)
    /// Compact button label (Quit, header controls).
    static let button = Font.system(size: 12, weight: .medium)
    /// Small status line, e.g. the transient toast.
    static let status = Font.system(size: 11, weight: .regular)
    /// Tabular numbers for live speeds and latency, so digits do not jitter as
    /// values change. Monospaced DIGITS only — the rest of the glyphs stay
    /// proportional, so the text does not look like code.
    static let numeric = Font.system(size: 12, weight: .medium).monospacedDigit()
    /// Large readout for the connected speed.
    static let speedReadout = Font.system(size: 15, weight: .medium).monospacedDigit()
    /// Primary status line (the connection state), one step above a row title.
    static let statusPrimary = Font.system(size: 14, weight: .medium)
    /// Small glyph inside a row, e.g. a chevron or an inline symbol.
    static let inlineGlyph = Font.system(size: 11, weight: .semibold)
    /// Badge text: short, medium weight, used for "Active" and counts.
    static let badge = Font.system(size: 12, weight: .medium)
    /// Smallest inline hint, e.g. the "↻" affordance on the version row.
    static let microGlyph = Font.system(size: 10)
    /// Hero glyph on the About screen.
    static let heroGlyph = Font.system(size: 40)
    /// Product name on the About screen.
    static let productName = Font.system(size: 17, weight: .semibold)
}

/// Spacing and geometry tokens.
///
/// Extends the existing `Metrics` (in MenuRow.swift) rather than replacing it:
/// that type is referenced across every view, and a rename would have been a
/// large mechanical diff with no behavioural gain.
extension Metrics {
    /// Horizontal inset of a section's ROWS from the panel edge.
    static let contentInset: CGFloat = 10
    /// Extra inset of the section HEADER, on top of `contentInset`.
    ///
    /// The header needs to sit clearly outside the row geometry. Header text is
    /// aligned to the row's ICON edge rather than its background edge, so the
    /// label visually starts the same column as the icons below it — without
    /// this the header sits further left than the content it labels, which is
    /// the misalignment that made the old layout look accidental.
    ///
    /// Derived from `rowPaddingH` rather than hardcoded, so
    /// `contentInset + sectionHeaderInset` is by construction the column the row
    /// icons occupy. A 2pt drift is invisible in a single screenshot and obvious
    /// once two sections are compared, so the two values must not be free to
    /// diverge.
    static var sectionHeaderInset: CGFloat { rowPaddingH }
    /// Gap between a section header and its first row.
    static let headerToRowGap: CGFloat = 4
    /// Gap between rows inside one section.
    ///
    /// 2, not 0: rows draw a hover/selection background, and at 0 the rounded
    /// corners of adjacent rows touch, which reads as one tall block.
    static let rowGap: CGFloat = 2
    /// Gap between sections. Larger than `rowGap` so grouping is visible without
    /// a divider per row.
    static let groupSpacing: CGFloat = 14
    /// Top padding of page content, below the header divider.
    static let contentTopPadding: CGFloat = 10
    /// Bottom padding, so the last row is not flush against the panel edge.
    static let contentBottomPadding: CGFloat = 12
    /// Leading icon column width. Fixed so titles line up across rows whether or
    /// not a row has an icon.
    static let iconColumn: CGFloat = 18
    /// Icon point size.
    static let iconSize: CGFloat = 13
    /// Trailing chevron size.
    static let chevronSize: CGFloat = 10
}
