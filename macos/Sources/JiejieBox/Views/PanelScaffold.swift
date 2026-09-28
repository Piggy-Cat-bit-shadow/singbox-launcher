// PanelScaffold — the one navigation chrome for every screen.
//
// Why this exists: NavigationStack inside a MenuBarExtra window does not give a
// dependable way back on macOS. The system back affordance is a toolbar item
// that a borderless menu-bar panel has nowhere to draw, so a page could be
// entered and then have no visible exit. Every screen therefore draws its own
// header, and this type is that header plus the fixed-content contract.
//
// Layout invariant (see docs/MACOS_MENU_BAR.md):
//
//	┌──────────────────────────────────────┐
//	│ ‹ Title                          Quit │  ← fixed
//	├──────────────────────────────────────┤
//	│  scrollable page content             │  ← scrolls
//	└──────────────────────────────────────┘
//
// The header never scrolls, so Back and Quit cannot be scrolled out of reach.
// Home passes `onBack: nil` and shows no back control; every other screen
// passes one. Quit is always present, on every screen including Home.

import SwiftUI

struct PanelScaffold<Content: View>: View {
    let model: AppModel
    let title: String
    /// Nil on the root screen, which has nothing to go back to.
    var onBack: (() -> Void)?
    /// Optional trailing content in the header, left of Quit.
    var headerAccessory: AnyView?
    /// Whether the scaffold provides the vertical scroll.
    ///
    /// A page that owns its own scrolling list (Proxies, whose node list is the
    /// content) must set this false. Nesting two vertical ScrollViews gives the
    /// inner one an unbounded height so it never scrolls itself, produces double
    /// bounce at the edges, and makes the wheel behave differently depending on
    /// which view happens to be under the pointer.
    ///
    /// The invariant this exists to hold: ONE page, ONE vertical scroll owner.
    var scrollsContent: Bool
    /// Whether the scaffold draws the shared action feedback (error + success).
    ///
    /// True for every ordinary page. A page that shows the same messages itself
    /// — Home, which owns the richest banner layout — sets this false so the
    /// message is not drawn twice.
    var showsFeedback: Bool
    @ViewBuilder var content: () -> Content

    init(
        model: AppModel,
        title: String,
        onBack: (() -> Void)? = nil,
        headerAccessory: AnyView? = nil,
        scrollsContent: Bool = true,
        showsFeedback: Bool = true,
        @ViewBuilder content: @escaping () -> Content
    ) {
        self.model = model
        self.title = title
        self.onBack = onBack
        self.headerAccessory = headerAccessory
        self.scrollsContent = scrollsContent
        self.showsFeedback = showsFeedback
        self.content = content
    }

    var body: some View {
        VStack(spacing: 0) {
            // The header is outside any scroll view on every page, so Back and
            // Quit can never be scrolled out of reach.
            PanelHeader(model: model, title: title, onBack: onBack, accessory: headerAccessory)
            Divider()
            // THE FEEDBACK IS PART OF THE CHROME, NOT OF ANY ONE PAGE.
            //
            // Every command writes its outcome to `lastError` / `transientStatus`,
            // and only Home and Subscriptions ever drew them. On the other nine
            // screens a failed action looked like nothing happening at all: the
            // spinner flashed, the error was recorded, and the user was shown no
            // reason. A click that produces neither a visible result nor a visible
            // explanation is indistinguishable from a broken button.
            //
            // Drawing it here means a screen cannot forget: the surface belongs to
            // the navigation chrome every page already uses. It sits OUTSIDE the
            // scroll view, next to the header, so a failure cannot be scrolled out
            // of sight either.
            if showsFeedback {
                ActionFeedbackStrip(model: model)
            }
            if scrollsContent {
                ScrollView {
                    content()
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .scrollBounceBehavior(.basedOnSize)
            } else {
                // The page owns the scroll. It fills the remaining height, so
                // its list has a bounded frame to scroll within.
                content()
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
        }
    }
}

/// The shared success/failure strip every page shows.
///
/// One implementation, drawn by `PanelScaffold`, so the eleven screens cannot
/// disagree about whether an action's outcome is visible. Deliberately compact
/// and non-modal: it states what happened and gets out of the way, and it never
/// steals focus from the page.
///
/// The two messages have different lifetimes ON PURPOSE, and that difference is
/// preserved here rather than flattened: a success line is transient (it is
/// information about something that already worked), while a failure persists
/// until dismissed or superseded, because an error that disappears on a timer is
/// an error the user may never read.
struct ActionFeedbackStrip: View {
    let model: AppModel
    @Environment(\.localization) private var language

    var body: some View {
        VStack(spacing: 0) {
            if let error = model.lastError, !error.isEmpty {
                Banner(kind: .error, message: error) {
                    Button {
                        model.clearError()
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                    }
                    .buttonStyle(.plain)
                    .help(L.dismiss.tr(language))
                    .accessibilityLabel(L.dismiss.tr(language))
                }
            }
            if let status = model.transientStatus, !status.isEmpty {
                Banner(kind: .info, message: status) {
                    Button {
                        model.clearTransientStatus()
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                    }
                    .buttonStyle(.plain)
                    .help(L.dismiss.tr(language))
                    .accessibilityLabel(L.dismiss.tr(language))
                }
            }
        }
    }
}

/// The header bar: back control, title, accessory, Quit.
struct PanelHeader: View {
    let model: AppModel
    let title: String
    var onBack: (() -> Void)?
    var accessory: AnyView?
    @Environment(\.localization) private var language

    var body: some View {
        HStack(spacing: 6) {
            if let onBack {
                PanelIconButton(systemImage: "chevron.left",
                                help: L.back.tr(language),
                                action: onBack)
            }

            Text(title)
                .font(Typography.pageTitle)
                .lineLimit(1)
                .truncationMode(.tail)
                .padding(.leading, onBack == nil ? 4 : 0)

            Spacer(minLength: 8)

            if let accessory { accessory }

            QuitButton(model: model)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .frame(minHeight: 38)
    }
}

/// A compact toolbar-sized icon button.
///
/// Deliberately small and subtle — a macOS toolbar control, not the oversized
/// circular button an earlier revision used. The hit target is still padded to
/// a comfortable size, and hover/press give feedback, so "compact" does not
/// mean "hard to hit".
struct PanelIconButton: View {
    let systemImage: String
    var help: String = ""
    let action: () -> Void

    private var hover: HoverState { HoverStore.box(for: "panelicon:\(systemImage)") }

    var body: some View {
        Button(action: action) {
            Image(systemName: systemImage)
                .font(Typography.inlineGlyph)
                .frame(width: 22, height: 22)
                .contentShape(Rectangle())
                .background(
                    RoundedRectangle(cornerRadius: 5)
                        .fill(hover.isHovering ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
                )
        }
        .buttonStyle(MenuRowButtonStyle())
        .onHover { hover.isHovering = $0 }
        .help(help)
        .accessibilityLabel(help.isEmpty ? systemImage : help)
    }
}

/// The always-present quit control.
///
/// One shared implementation rather than a copy per screen: quit must behave
/// identically everywhere, and per-screen copies are how two behaviours
/// (graceful vs. force) drift apart.
///
/// It calls `model.quit()`, which sends `shutdown` and lets the BACKEND decide
/// whether the core stops. It must never pre-stop the core: in daemon mode with
/// keep-running set, stopping first would defeat the whole point.
struct QuitButton: View {
    let model: AppModel
    @Environment(\.localization) private var language

    private var hover: HoverState { HoverStore.box(for: "quit") }

    var body: some View {
        Button {
            Task { await model.quit() }
        } label: {
            // Quitting is a multi-step teardown that can take a moment, so the
            // control shows progress instead of looking inert — and is disabled
            // meanwhile, so five impatient clicks cannot start five teardowns.
            if model.isQuitting {
                HStack(spacing: 5) {
                    ProgressView().controlSize(.mini)
                    Text(L.quitting.tr(language))
                        .font(Typography.button)
                }
                .padding(.horizontal, 8)
                .frame(height: 22)
            } else {
                Text(L.quit.tr(language))
                    .font(Typography.button)
                    .padding(.horizontal, 8)
                    .frame(height: 22)
                    .contentShape(Rectangle())
                    .background(
                        RoundedRectangle(cornerRadius: 5)
                            .fill(hover.isHovering ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
                    )
            }
        }
        .buttonStyle(MenuRowButtonStyle())
        .onHover { hover.isHovering = $0 }
        .disabled(model.isQuitting)
        .help(L.quitHelp.tr(language))
        .accessibilityLabel(L.quitHelp.tr(language))
    }
}
