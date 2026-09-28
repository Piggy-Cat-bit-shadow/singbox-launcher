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
/// A reference type rather than @State because this toolchain ships the
/// Observation macro plugin but NOT the SwiftUI one, so @State cannot compile
/// (verified: "external macro implementation type 'SwiftUIMacros.StateMacro'
/// could not be found"). @Observable gives the re-render on change without a
/// property wrapper.
///
/// Crucially it must NOT be created per view instantiation: SwiftUI builds a
/// new struct value on every parent body pass, so a stored
/// `let hover = HoverState()` would be replaced each time and the hover tint
/// would flicker or stick. Rows therefore take their hover box from
/// `HoverStore`, which keeps one box per row identity for the process
/// lifetime.
@Observable
final class HoverState {
    var isHovering = false
}

/// Keeps one `HoverState` per row identity.
///
/// Identity is the row's stable key (title plus any distinguishing value), not
/// the struct instance — which is exactly what survives a body re-evaluation.
/// Bounded because a panel has a few dozen rows; the cap only guards against
/// unbounded growth if a list with generated keys is ever added.
@MainActor
enum HoverStore {
    private static var boxes: [String: HoverState] = [:]
    private static let cap = 512

    static func box(for key: String) -> HoverState {
        if let existing = boxes[key] { return existing }
        if boxes.count >= cap { boxes.removeAll() }
        let box = HoverState()
        boxes[key] = box
        return box
    }
}

/// Visual role of a row.
enum MenuRowRole {
    /// Ordinary navigation or action.
    case normal
    /// Destructive or attention-drawing action.
    case destructive
}

struct MenuRow<Trailing: View>: View {
    /// Stable hover key: the row's identity, not the struct instance.
    private var hover: HoverState { HoverStore.box(for: hoverKey) }
    /// The row's hover identity.
    ///
    /// The DISPLAYED TEXT is only a fallback. It is not an identity: two rows can
    /// legitimately read the same — the Daemon screen shows "Refresh Status" in
    /// both its status section and its command section — and keying on the text
    /// made them share one hover box, so pointing at either tinted both. A row
    /// that needs to be told apart passes an explicit `hoverID`.
    private var hoverKey: String {
        if let hoverID { return "menurow:\(hoverID)" }
        return "menurow:\(title)|\(subtitle ?? "")|\(value ?? "")"
    }
    /// Explicit hover identity, for rows the title cannot distinguish.
    var hoverID: String?
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
        hoverID: String? = nil,
        action: @escaping () -> Void,
        @ViewBuilder trailing: @escaping () -> Trailing = { EmptyView() }
    ) {
        self.hoverID = hoverID
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
            HStack(spacing: Metrics.iconToTextSpacing) {
                if let systemImage {
                    Image(systemName: systemImage)
                        .font(.system(size: Metrics.iconSize))
                        .frame(width: Metrics.iconColumn)
                        .foregroundStyle(role == .destructive ? Color.red : .secondary)
                }

                VStack(alignment: .leading, spacing: 1) {
                    Text(title)
                        .font(Typography.rowTitle)
                        .lineLimit(1)
                        .truncationMode(.tail)
                        .foregroundStyle(role == .destructive ? Color.red : .primary)
                    if let subtitle {
                        Text(subtitle)
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            // Two lines, but the row does not grow to fit an
                            // arbitrarily long message: a translated subtitle
                            // can be much longer than the English one, and an
                            // unbounded row makes the whole section look broken
                            // in that language.
                            .lineLimit(2)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }

                Spacer(minLength: 8)

                if let value {
                    Text(value)
                        .font(Typography.rowValue)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }

                trailing()

                if showsChevron {
                    Image(systemName: "chevron.right")
                        .font(.system(size: Metrics.chevronSize, weight: .semibold))
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
    /// Comfortable minimum hit height.
    ///
    /// 34, down from 38: the panel was airy rather than dense, and the
    /// per-section rhythm is fixed by the tokens in Typography.swift, so the row
    /// height is what actually sets the page's density. 34pt is still a
    /// comfortable target — above the 28pt macOS minimum — and the row spans the
    /// full panel width, so the hit AREA stays large even as the height drops.
    static let rowHeight: CGFloat = 34
    static let rowPaddingH: CGFloat = 12
    static let rowCorner: CGFloat = 6
    /// Padding around a section's rows.
    static let sectionPaddingH: CGFloat = 6
    static let sectionSpacing: CGFloat = 4
    static let panelWidth: CGFloat = 400
}

/// A titled group of rows.
///
/// The header is aligned to the ROW's TITLE column — the same column the words
/// under it start in — via `Metrics.sectionHeaderInset`. Aligning it to the icon
/// column instead (the previous behaviour) left every header one icon-plus-gap
/// too far left, so it read as a caption belonging to nothing. Done with padding
/// rather than a fixed frame width, so it holds at any panel size and in either
/// language.
///
/// No hairline separator any more: with `Metrics.groupSpacing` larger than
/// `Metrics.rowGap`, whitespace alone expresses the grouping. A rule between
/// every section and around every row was most of what made the old panel look
/// like a crowded table rather than a native menu.
struct MenuSection<Content: View>: View {
    let title: String?
    @ViewBuilder var content: () -> Content

    init(_ title: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.content = content
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Metrics.headerToRowGap) {
            if let title {
                Text(title)
                    .font(Typography.sectionHeader)
                    .foregroundStyle(.secondary)
                    .textCase(nil)
                    .lineLimit(1)
                    .truncationMode(.tail)
                    .padding(.leading, Metrics.sectionHeaderInset)
                    .padding(.bottom, 1)
                    .accessibilityAddTraits(.isHeader)
            }
            VStack(spacing: Metrics.rowGap) {
                content()
            }
        }
        .padding(.horizontal, Metrics.contentInset)
    }
}

/// A read-only label/value line.
///
/// Not a row: it is not interactive, and rendering it as a button would create
/// exactly the "looks clickable but does nothing" defect the row primitives
/// exist to avoid. Lives here beside them because several screens use it.
///
/// Not a MenuRow: it is not interactive, and rendering it as a button would
/// create exactly the "looks clickable but does nothing" defect the row
/// primitives exist to avoid.
struct DetailLine: View {
    enum Tone { case normal, error }

    let label: String
    let value: String
    var tone: Tone = .normal

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            // The label yields first: values such as a core version, an
            // endpoint or an error message are the information, while the label
            // is a fixed caption that is meaningless when truncated.
            Text(label)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .layoutPriority(-1)
            Spacer(minLength: 8)
            Text(value)
                .font(Typography.rowValue)
                .foregroundStyle(tone == .error ? Color.red : Color.primary)
                .multilineTextAlignment(.trailing)
                .lineLimit(3)
                .truncationMode(.middle)
                .textSelection(.enabled)
                // The full value stays reachable even when it is clipped, so a
                // long path or error is never lost to the panel width.
                .help(value)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 5)
    }
}

/// A row whose root IS a `Menu`, for choosing one of a few options.
///
/// Why this exists rather than a `Menu` inside `MenuRow`: MenuRow's root is a
/// Button, so putting a Menu in its label nests two interactive controls — the
/// defect the row primitives were written to eliminate. Here the Menu is the
/// only control, and the visual layout is shared with MenuRow so the row still
/// looks like every other row.
///
/// `Menu` rather than `Picker` for the same reason: a Picker in a menu-bar panel
/// renders as its own control with its own hit area, and it would sit inside the
/// row rather than being the row.
struct MenuPickerRow<Option: Hashable & Identifiable>: View {
    let title: String
    var subtitle: String?
    var systemImage: String?
    let options: [Option]
    let selection: Option
    let label: (Option) -> String
    let onSelect: (Option) -> Void
    /// Row-level disablement, e.g. while an unrelated operation is running.
    var disabled: Bool = false

    private var hover: HoverState { HoverStore.box(for: "menupicker:\(title)") }

    var body: some View {
        Menu {
            ForEach(options) { option in
                Button {
                    onSelect(option)
                } label: {
                    // A checkmark marks the active choice, matching the group
                    // picker on the Proxies screen.
                    if option == selection {
                        Label(label(option), systemImage: "checkmark")
                    } else {
                        Text(label(option))
                    }
                }
            }
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
                    if let subtitle {
                        Text(subtitle)
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            .lineLimit(2)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }

                Spacer(minLength: 8)

                Text(label(selection))
                    .font(Typography.rowValue)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)

                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: Metrics.chevronSize, weight: .semibold))
                    .foregroundStyle(.tertiary)
            }
            // Same full-width hit target and height as MenuRow, so the row reads
            // as the same kind of thing and the whole width is clickable.
            .frame(maxWidth: .infinity, minHeight: Metrics.rowHeight, alignment: .leading)
            .padding(.horizontal, Metrics.rowPaddingH)
            .contentShape(Rectangle())
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(hover.isHovering && !disabled
                          ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
        }
        .menuStyle(.borderlessButton)
        .menuIndicator(.hidden)
        .disabled(disabled)
        .onHover { hover.isHovering = $0 }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(title): \(label(selection))")
    }
}

/// One choice inside a `MenuActionRow`.
///
/// An explicit id rather than the title: two entries may legitimately share a
/// title, and the id is what the action closure switches on.
struct MenuAction: Identifiable {
    let id: String
    let title: String
    let systemImage: String?
    let disabled: Bool
    let action: () -> Void

    init(id: String, title: String, systemImage: String? = nil,
         disabled: Bool = false, action: @escaping () -> Void) {
        self.id = id
        self.title = title
        self.systemImage = systemImage
        self.disabled = disabled
        self.action = action
    }
}

/// A row whose root IS a `Menu`, offering a list of ACTIONS.
///
/// Distinct from `MenuPickerRow`, which picks one value out of a set and shows
/// the current choice in the row: here the entries are verbs ("Add from URL…",
/// "Import from File…") and there is no selected state to display. Reusing the
/// picker for this would render a "selection" that does not exist.
///
/// The Menu is the row's only control, for the reason documented on
/// MenuPickerRow: a Button wrapping a Menu nests two interactive controls, so
/// the inner one becomes hit-disabled and appears broken.
struct MenuActionRow: View {
    let title: String
    var subtitle: String?
    var systemImage: String?
    let actions: [MenuAction]
    /// Row-level disablement, e.g. while an unrelated operation is running.
    var disabled: Bool = false

    private var hover: HoverState { HoverStore.box(for: "menuaction:\(title)") }

    var body: some View {
        Menu {
            ForEach(actions) { action in
                Button {
                    action.action()
                } label: {
                    if let image = action.systemImage {
                        Label(action.title, systemImage: image)
                    } else {
                        Text(action.title)
                    }
                }
                .disabled(action.disabled)
            }
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
                    if let subtitle {
                        Text(subtitle)
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            .lineLimit(2)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }

                Spacer(minLength: 8)

                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: Metrics.chevronSize, weight: .semibold))
                    .foregroundStyle(.tertiary)
            }
            // Same full-width hit target and height as MenuRow, so the row reads
            // as the same kind of thing and the whole width is clickable.
            .frame(maxWidth: .infinity, minHeight: Metrics.rowHeight, alignment: .leading)
            .padding(.horizontal, Metrics.rowPaddingH)
            .contentShape(Rectangle())
            .background(
                RoundedRectangle(cornerRadius: Metrics.rowCorner)
                    .fill(hover.isHovering && !disabled
                          ? AnyShapeStyle(.quaternary) : AnyShapeStyle(.clear))
            )
        }
        .menuStyle(.borderlessButton)
        .menuIndicator(.hidden)
        .disabled(disabled)
        .onHover { hover.isHovering = $0 }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(title)
    }
}
