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
    /// Disables THIS action only.
    ///
    /// A row can have actions with different availability: while one node is
    /// being tested, its own select action must not be clickable either — the
    /// pair is inconsistent mid-measurement — yet other rows stay usable. A
    /// row-wide flag cannot express that, and relying on the model's
    /// `withPending` guard instead means the user clicks a live-looking control
    /// and is told an operation is already running.
    var isDisabled: Bool
    /// Leading content shown instead of title/subtitle when present.
    var leading: AnyView?
    /// Fraction of the row this action occupies. The primary action should
    /// always get the larger share.
    var weight: CGFloat
    var help: String?
    /// Spoken label, when the visual title cannot serve as one.
    ///
    /// The latency control draws a number and leaves `title` empty, so without
    /// this its accessibility label was the empty string. Nil means "the title
    /// is the label", which is right for text actions.
    var accessibilityLabel: String?
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
        isDisabled: Bool = false,
        leading: AnyView? = nil,
        weight: CGFloat = 1,
        help: String? = nil,
        accessibilityLabel: String? = nil,
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
        self.isDisabled = isDisabled
        self.leading = leading
        self.weight = weight
        self.help = help
        self.accessibilityLabel = accessibilityLabel
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

    /// The action is usable only when neither the row nor the action itself is
    /// disabled.
    private var isEnabled: Bool { !disabled && !action.isDisabled }

    var body: some View {
        Button(action: action.action) {
            HStack(spacing: Metrics.iconToTextSpacing) {
                if let leading = action.leading {
                    leading
                } else if let systemImage = action.systemImage {
                    Image(systemName: systemImage)
                        .font(Typography.rowTitle)
                        .frame(width: Metrics.iconColumn)
                        .foregroundStyle(action.role == .destructive ? Color.red : .secondary)
                }

                VStack(alignment: .leading, spacing: 1) {
                    Text(action.title)
                        .foregroundStyle(action.role == .destructive ? Color.red : .primary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                    if let subtitle = action.subtitle {
                        Text(subtitle)
                            .font(Typography.rowSubtitle)
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
                        .font(Typography.numeric)
                        .foregroundStyle(action.valueColor ?? .secondary)
                        .lineLimit(1)
                }

                if action.showsChevron {
                    Image(systemName: "chevron.right")
                        .font(Typography.inlineGlyph)
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
        // Hover feedback is for a control the user CAN use. This read
        // `$0 && !isEnabled`, which inverted it: an ENABLED action never lit up
        // under the pointer, and a DISABLED one highlighted as though it were
        // live — the exact opposite of what hover is for on both counts.
        .onHover { hover.isHovering = $0 && isEnabled }
        .disabled(!isEnabled)
        .help(action.help ?? "")
        .accessibilityElement(children: .combine)
        // An action may carry a label for assistive technology that differs from
        // what it draws. The latency control is the case that forced this: its
        // visual title is an empty string (the row already shows the number), so
        // `accessibilityLabel(action.title)` published an EMPTY label and
        // VoiceOver could not describe the control at all.
        .accessibilityLabel(action.accessibilityLabel ?? action.title)
    }
}
