// ActionRow — a row with two or more REAL actions, as sibling controls.
//
// MenuRow is for "one row = one action". When a row genuinely has two actions
// (a proxy row both selects the node and measures its latency), embedding the
// second control in MenuRow's `trailing` slot puts an interactive control inside
// a Button's label. That nesting is not a style choice, it is a bug:
//
//   - the child button's hit region competes with the parent's;
//   - clicking the child can also fire the parent (switching the proxy while
//     the user only asked to measure it);
//   - hover and pressed states fight, so neither reads correctly;
//   - the accessibility tree reports a button inside a button.
//
// ActionRow therefore lays its actions out as SIBLINGS in an HStack. Each is a
// real Button with its own full-height hit region, separated by 0 spacing so
// there is no dead gap between them.

import SwiftUI

/// One action inside an ActionRow.
struct RowAction: Identifiable {
    let id: String
    let title: String
    var subtitle: String?
    var systemImage: String?
    /// Trailing value text, e.g. a latency reading.
    var value: String?
    var valueColor: Color?
    var showsChevron: Bool
    var role: MenuRowRole
    /// True while this particular action is in flight.
    var isPending: Bool
    /// Leading content shown instead of title/subtitle when present.
    var leading: AnyView?
    /// Fraction of the row this action occupies. The primary action should
    /// always get the larger share.
    var weight: CGFloat
    var help: String?
    var action: () -> Void

    init(
        id: String,
        title: String,
        subtitle: String? = nil,
        systemImage: String? = nil,
        value: String? = nil,
        valueColor: Color? = nil,
        showsChevron: Bool = false,
        role: MenuRowRole = .normal,
        isPending: Bool = false,
        leading: AnyView? = nil,
        weight: CGFloat = 1,
        help: String? = nil,
        action: @escaping () -> Void
    ) {
        self.id = id
        self.title = title
        self.subtitle = subtitle
        self.systemImage = systemImage
        self.value = value
        self.valueColor = valueColor
        self.showsChevron = showsChevron
        self.role = role
        self.isPending = isPending
        self.leading = leading
        self.weight = weight
        self.help = help
        self.action = action
    }
}

/// A row whose actions are independent, non-nested controls.
struct ActionRow: View {
    let actions: [RowAction]
    /// Disables every action without changing their appearance to "gone".
    var disabled: Bool = false

    var body: some View {
        HStack(spacing: 0) {
            ForEach(actions) { action in
                ActionRowButton(action: action, disabled: disabled)
            }
        }
        .padding(.horizontal, Metrics.sectionPaddingH)
    }
}

/// A single action button inside an ActionRow.
private struct ActionRowButton: View {
    let action: RowAction
    let disabled: Bool

    /// Stable per-action hover key, so the tint survives a body re-evaluation.
    private var hover: HoverState { HoverStore.box(for: "actionrow:\(action.id)") }

    var body: some View {
        Button(action: action.action) {
            HStack(spacing: 10) {
                if let leading = action.leading {
                    leading
                } else if let systemImage = action.systemImage {
                    Image(systemName: systemImage)
                        .font(.system(size: 13))
                        .frame(width: 18)
                        .foregroundStyle(action.role == .destructive ? Color.red : .secondary)
                }

                VStack(alignment: .leading, spacing: 1) {
                    Text(action.title)
                        .foregroundStyle(action.role == .destructive ? Color.red : .primary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    if let subtitle = action.subtitle {
                        Text(subtitle)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.tail)
                    }
                }

                Spacer(minLength: 6)

                if action.isPending {
                    ProgressView().controlSize(.small)
                } else if let value = action.value {
                    Text(value)
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(action.valueColor ?? .secondary)
                        .lineLimit(1)
                }

                if action.showsChevron {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(.tertiary)
                }
            }
            // Every action fills its share of the row and the full row height,
            // so both hit regions are large and their boundary is exact.
            .frame(maxWidth: .infinity, minHeight: Metrics.rowHeight, alignment: .leading)
            .padding(.horizontal, Metrics.rowPaddingH)
            .contentShape(Rectangle())
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(hover.isHovering ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
        }
        .buttonStyle(MenuRowButtonStyle())
        .layoutPriority(action.weight)
        .onHover { hover.isHovering = $0 && !disabled }
        .disabled(disabled)
        .help(action.help ?? "")
        .accessibilityElement(children: .combine)
        .accessibilityLabel(action.title)
    }
}
