// MenuRow — the single interactive row primitive for the whole app.
//
// Every clickable row goes through this component so hit targets, hover and
// pressed feedback are consistent rather than re-invented per view. The three
// defects it exists to prevent:
//
//   1. Rows that only responded near their text. The label is built with
//      `.frame(maxWidth: .infinity, alignment: .leading)` and
//      `.contentShape(Rectangle())`, so the whole row width is hittable.
//   2. Rows with no visible reaction. Hover tints the background and the
//      style dims it while pressed.
//   3. Rows too short to hit comfortably. The minimum height is enforced here,
//      independent of how compact the visual padding looks.
//
// It is always a Button: no `Text.onTapGesture`, no transparent overlay.

import Observation
import SwiftUI

/// Per-row hover flag.
///
/// A reference type rather than @State: this toolchain has the Observation
/// macro plugin but not the SwiftUI one, so @State cannot compile (see
/// AppModel for the same constraint). @Observable gives the view the
/// re-render on change without a property wrapper.
@Observable
final class HoverState {
    var isHovering = false
}

/// Visual role of a row.
enum MenuRowRole {
    /// Ordinary navigation or action.
    case normal
    /// Destructive or attention-drawing action.
    case destructive
}

struct MenuRow<Trailing: View>: View {
    private let hover = HoverState()
    let title: String
    var subtitle: String?
    var systemImage: String?
    var value: String?
    var showsChevron: Bool
    var role: MenuRowRole
    var action: () -> Void
    @ViewBuilder var trailing: () -> Trailing

    init(
        _ title: String,
        subtitle: String? = nil,
        systemImage: String? = nil,
        value: String? = nil,
        showsChevron: Bool = false,
        role: MenuRowRole = .normal,
        action: @escaping () -> Void,
        @ViewBuilder trailing: @escaping () -> Trailing = { EmptyView() }
    ) {
        self.title = title
        self.subtitle = subtitle
        self.systemImage = systemImage
        self.value = value
        self.showsChevron = showsChevron
        self.role = role
        self.action = action
        self.trailing = trailing
    }

    var body: some View {
        Button(action: action) {
            HStack(spacing: 10) {
                if let systemImage {
                    Image(systemName: systemImage)
                        .font(.system(size: 13))
                        .frame(width: 18)
                        .foregroundStyle(role == .destructive ? Color.red : .secondary)
                }

                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                        .foregroundStyle(role == .destructive ? Color.red : .primary)
                    if let subtitle {
                        Text(subtitle)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(2)
                    }
                }

                Spacer(minLength: 8)

                if let value {
                    Text(value)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }

                trailing()

                if showsChevron {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(.tertiary)
                }
            }
            // Full-width hit target, minimum comfortable height.
            .frame(maxWidth: .infinity, minHeight: Metrics.rowHeight, alignment: .leading)
            .padding(.horizontal, Metrics.rowPaddingH)
            .contentShape(Rectangle())
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(hover.isHovering ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
        }
        .buttonStyle(MenuRowButtonStyle())
        .onHover { hover.isHovering = $0 }
        .accessibilityElement(children: .combine)
        .accessibilityHint(showsChevron ? "Opens a submenu" : "")
    }
}

extension MenuRow where Trailing == EmptyView {
    init(
        _ title: String,
        subtitle: String? = nil,
        systemImage: String? = nil,
        value: String? = nil,
        showsChevron: Bool = false,
        role: MenuRowRole = .normal,
        action: @escaping () -> Void
    ) {
        self.init(
            title, subtitle: subtitle, systemImage: systemImage, value: value,
            showsChevron: showsChevron, role: role, action: action,
            trailing: { EmptyView() })
    }
}

/// Pressed feedback for rows.
///
/// `.plain` gives no visual response at all, which is why clicks felt
/// unacknowledged. This dims the label and adds a slightly stronger fill while
/// the mouse is down — the standard macOS row behaviour, with no scale or
/// spring animation.
struct MenuRowButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(configuration.isPressed
                          ? AnyShapeStyle(.selection.opacity(0.35))
                          : AnyShapeStyle(.clear))
            )
            .opacity(configuration.isPressed ? 0.75 : 1)
    }
}

/// Shared layout constants, so every row and section lines up.
enum Metrics {
    /// Comfortable minimum hit height. Below this the row is hard to hit even
    /// when the visual padding is tight.
    static let rowHeight: CGFloat = 38
    static let rowPaddingH: CGFloat = 12
    static let rowCorner: CGFloat = 6
    /// Padding around a section's rows.
    static let sectionPaddingH: CGFloat = 6
    static let sectionSpacing: CGFloat = 4
    static let panelWidth: CGFloat = 400
}

/// A titled group of rows, drawing its own hairline separator.
struct MenuSection<Content: View>: View {
    let title: String?
    @ViewBuilder var content: () -> Content

    init(_ title: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.content = content
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.sectionSpacing) {
            if let title {
                Text(title)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, Metrics.rowPaddingH)
                    .padding(.top, 2)
            }
            VStack(spacing: 1) {
                content()
            }
        }
        .padding(.horizontal, Metrics.sectionPaddingH)
    }
}
