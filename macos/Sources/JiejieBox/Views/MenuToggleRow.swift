// MenuToggleRow — the one boolean-setting row.
//
// Extracted from MoreView, where Auto Ping, Auto Update and Launch at Login each
// had their own near-copy of the same layout. Three copies is how a panel ends up
// with rows that differ by a pixel in height, a slightly different subtitle font,
// or a switch that sits at a different offset — the "functionally fine, visually
// unfinished" quality this pass is meant to remove.
//
// Design decisions that matter:
//
//   * The SWITCH IS NOT INTERACTIVE (`.allowsHitTesting(false)`) and the whole
//     row is the button. A bare switch is a small target; a row is not. Clicking
//     anywhere on the row flips the setting, which is both easier to hit and what
//     a native settings row does.
//   * `pending` shows a spinner IN PLACE OF the switch rather than beside it, so
//     the trailing column does not change width while saving and the row does not
//     visibly jump.
//   * The title and switch are vertically centred against each other regardless
//     of whether a subtitle is present, so rows with and without a subtitle line
//     up.

import SwiftUI

struct MenuToggleRow: View {
    let title: String
    var subtitle: String?
    var systemImage: String?
    let isOn: Bool
    /// True while this specific setting is being written.
    var pending: Bool = false
    /// Row-level disablement, e.g. while an unrelated backend call is in flight.
    var disabled: Bool = false
    var help: String?
    let set: (Bool) -> Void

    private var hover: HoverState { HoverStore.box(for: "menutoggle:\(title)") }

    var body: some View {
        Button {
            set(!isOn)
        } label: {
            HStack(spacing: Metrics.iconToTextSpacing) {
                if let systemImage {
                    Image(systemName: systemImage)
                        .font(.system(size: Metrics.iconSize))
                        .frame(width: Metrics.iconColumn)
                        .foregroundStyle(.secondary)
                }

                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                        .font(Typography.rowTitle)
                        .lineLimit(1)
                        .truncationMode(.tail)
                    if let subtitle {
                        Text(subtitle)
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            // A translated subtitle can be noticeably longer than
                            // the English one. Two lines and a fixed vertical size
                            // keep the row from growing without bound in either
                            // language.
                            .lineLimit(2)
                            .fixedSize(horizontal: false, vertical: true)
                            .multilineTextAlignment(.leading)
                    }
                }

                Spacer(minLength: 8)

                // Fixed-width trailing column: the spinner and the switch occupy
                // the same box, so saving does not shift the layout.
                Group {
                    if pending {
                        ProgressView().controlSize(.small)
                    } else {
                        Toggle("", isOn: Binding(get: { isOn }, set: set))
                            .labelsHidden()
                            .toggleStyle(.switch)
                            .controlSize(.small)
                            .allowsHitTesting(false)
                    }
                }
                .frame(width: 38, alignment: .trailing)
            }
            .frame(maxWidth: .infinity, minHeight: Metrics.rowHeight, alignment: .leading)
            .padding(.horizontal, Metrics.rowPaddingH)
            .contentShape(Rectangle())
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(hover.isHovering && !disabled
                          ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
        }
        .buttonStyle(MenuRowButtonStyle())
        .onHover { hover.isHovering = $0 }
        .disabled(disabled)
        .help(help ?? subtitle ?? title)
        // One accessibility element describing the setting and its state, rather
        // than a button plus a separate unlabelled switch.
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(title)
        .accessibilityValue(isOn ? "on" : "off")
        .accessibilityAddTraits(isOn ? [.isButton, .isSelected] : .isButton)
    }
}
