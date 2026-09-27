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
    @ViewBuilder var content: () -> Content

    init(
        model: AppModel,
        title: String,
        onBack: (() -> Void)? = nil,
        headerAccessory: AnyView? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) {
        self.model = model
        self.title = title
        self.onBack = onBack
        self.headerAccessory = headerAccessory
        self.content = content
    }

    var body: some View {
        VStack(spacing: 0) {
            PanelHeader(model: model, title: title, onBack: onBack, accessory: headerAccessory)
            Divider()
            ScrollView {
                content()
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .scrollBounceBehavior(.basedOnSize)
        }
    }
}

/// The header bar: back control, title, accessory, Quit.
struct PanelHeader: View {
    let model: AppModel
    let title: String
    var onBack: (() -> Void)?
    var accessory: AnyView?

    var body: some View {
        HStack(spacing: 6) {
            if let onBack {
                PanelIconButton(systemImage: "chevron.left",
                                help: "Back",
                                action: onBack)
            }

            Text(title)
                .font(.headline)
                .lineLimit(1)
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
                .font(.system(size: 12, weight: .semibold))
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
                    Text("Quitting…")
                        .font(.caption.weight(.medium))
                }
                .padding(.horizontal, 8)
                .frame(height: 22)
            } else {
                Text("Quit")
                    .font(.caption.weight(.medium))
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
        .help("Quit JiejieBox")
        .accessibilityLabel("Quit JiejieBox")
    }
}
