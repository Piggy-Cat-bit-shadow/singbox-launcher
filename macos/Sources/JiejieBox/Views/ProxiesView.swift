// ProxiesView — pick a group, pick a node, see its latency.
//
// Layout, top to bottom, mirroring the questions a user actually has:
//
//   group toolbar   which group am I in, and can I test it?
//   search          narrow the list
//   body            exactly one state: loading / stopped / empty / stale /
//                   unavailable / error / nodes
//
// The body renders ONE state, chosen by `model.proxyListState`. The earlier
// version collapsed every "no nodes" cause into a single sentence, so a stopped
// core, an empty config and an unreachable backend all looked identical — and
// only one of those messages could ever be right.
//
// Interaction note: a node row has two independent actions, so both are real
// Buttons laid out as siblings (see ActionRow). Nothing is nested, and neither
// action can trigger the other.

import SwiftUI

struct ProxiesView: View {
    let model: AppModel

    var body: some View {
        // scrollsContent: false — this page's node list is the scroll owner.
        // The scaffold would otherwise wrap it in a second vertical ScrollView.
        PanelScaffold(model: model, title: "Proxies", onBack: { model.goBack() },
                      scrollsContent: false) {
            VStack(alignment: .leading, spacing: 0) {
                // The toolbar only appears once there is something to control:
                // with no groups or a dead backend, an empty picker and a
                // disabled Test All would be decoration.
                if showsToolbar {
                    groupToolbar
                    Divider().padding(.horizontal, Metrics.sectionPaddingH)
                }

                if showsSearch {
                    searchField
                    Divider().padding(.horizontal, Metrics.sectionPaddingH)
                }

                body_
                    .padding(.vertical, 6)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
        }
        .task {
            // Loading here (rather than on appear-and-forget) means the spinner
            // reflects the real request lifetime. An existing list is refreshed
            // rather than re-fetched blind, so returning to the screen is cheap.
            if model.groups.isEmpty {
                await model.loadGroups()
            } else {
                await model.loadProxies()
            }
        }
    }

    // MARK: - What to show

    private var showsToolbar: Bool {
        switch model.proxyListState {
        case .loading, .noGroups, .idle, .backendUnavailable: return false
        default: return !model.groups.isEmpty
        }
    }

    private var showsSearch: Bool {
        model.proxyListState == .ready || model.proxyListState == .empty
    }

    // MARK: - Group toolbar

    private var groupToolbar: some View {
        HStack(spacing: 8) {
            groupPicker
            Spacer(minLength: 8)
            testAllButton
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 6)
        .frame(minHeight: 40)
    }

    private var groupPicker: some View {
        Menu {
            ForEach(model.groups) { group in
                Button {
                    Task { await model.selectGroup(group.name) }
                } label: {
                    // The checkmark marks the group in use; the label shows the
                    // node it currently points at, which is the fact the user
                    // actually wants from this menu.
                    if group.name == model.selectedGroup {
                        Label(groupSelectionLabel(group), systemImage: "checkmark")
                    } else {
                        Text(groupSelectionLabel(group))
                    }
                }
            }
        } label: {
            HStack(spacing: 5) {
                Image(systemName: "square.stack.3d.up")
                    .font(.system(size: 12))
                    .foregroundStyle(.secondary)
                Text(currentGroupLabel)
                    .font(.callout.weight(.medium))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: 9, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
        .help(model.groups.count > 1
              ? "Switch group. \(model.groups.count) groups available."
              : "The active selector group.")
    }

    /// A real button with its own hit area, not a floating label. Disabled with
    /// a reason in the tooltip when there is nothing to test or the backend
    /// cannot run a test.
    private var testAllButton: some View {
        Button {
            Task { await model.testGroup() }
        } label: {
            HStack(spacing: 5) {
                if model.pending == .testingGroup {
                    ProgressView().controlSize(.mini)
                } else {
                    Image(systemName: "bolt.horizontal")
                        .font(.system(size: 11))
                }
                Text(model.pending == .testingGroup ? "Testing…" : "Test All")
                    .font(.caption.weight(.medium))
            }
            .padding(.horizontal, 9)
            .frame(height: 24)
            .contentShape(Rectangle())
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
        .disabled(!canTestGroup)
        .help(testAllHelp)
    }

    private var canTestGroup: Bool {
        guard model.proxyListState == .ready else { return false }
        return model.pending == nil
    }

    private var testAllHelp: String {
        switch model.proxyListState {
        case .coreStopped: return "Start the core to test latency."
        case .backendUnavailable: return "The backend is unavailable."
        case .empty: return "This group has no nodes to test."
        case .ready:
            return model.pending == nil
                ? "Measure latency for every node in this group."
                : "Another operation is running."
        default: return "Nothing to test yet."
        }
    }

    private func groupSelectionLabel(_ group: ProxyGroup) -> String {
        guard let selected = group.selected_display ?? group.selected, !selected.isEmpty else {
            return group.label
        }
        return "\(group.label) — \(selected)"
    }

    private var currentGroupLabel: String {
        let group = model.groups.first { $0.name == model.selectedGroup }
        let base = group?.label ?? (model.selectedGroup.isEmpty ? "No group" : model.selectedGroup)
        guard let selected = group?.selected_display ?? group?.selected, !selected.isEmpty else {
            return base
        }
        return "\(base) — \(selected)"
    }

    // MARK: - Search

    private var searchField: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .font(.system(size: 11))
                .foregroundStyle(.secondary)
            TextField("Search nodes", text: Binding(get: { model.proxySearch },
                                                    set: { model.proxySearch = $0 }))
                .textFieldStyle(.plain)
                .font(.callout)
            // Clearing matters on a filtered list: without it the only way back
            // to the full list is selecting and deleting the text by hand.
            if !model.proxySearch.isEmpty {
                Button {
                    model.proxySearch = ""
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help("Clear the search.")
            }
            if !model.proxies.isEmpty {
                Text(countLabel)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .monospacedDigit()
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 7)
    }

    private var countLabel: String {
        let total = model.proxies.count
        let shown = model.filteredProxies.count
        if shown == total { return total == 1 ? "1 node" : "\(total) nodes" }
        return "\(shown) of \(total)"
    }

    // MARK: - Body — exactly one state

    @ViewBuilder
    private var body_: some View {
        switch model.proxyListState {
        case .backendUnavailable:
            BackendDownView(model: model, subject: "the proxy list")

        case .loading, .idle:
            loadingRow

        case .coreStopped:
            ProxyNotice(
                symbol: "power",
                title: "Core is not running",
                detail: "Start the core to load, test and switch nodes.",
                tone: .neutral)

        case .failed:
            ProxyNotice(
                symbol: "exclamationmark.triangle",
                title: "Could not load proxies",
                detail: model.proxyError ?? "The backend did not answer.",
                tone: .error,
                action: ("Try Again", { Task { await model.loadGroups() } }))

        case .configStale:
            // The offered action depends on who owns the config. Pointing an
            // external-config user at "Reload Config" would lead to an action
            // that can only fail; they get the file instead.
            if model.configRebuildable {
                ProxyNotice(
                    symbol: "arrow.triangle.2.circlepath",
                    title: "Configuration needs reload",
                    detail: "Subscriptions changed, so the node list is out of date.",
                    tone: .warning,
                    action: ("Reload Config", { reloadConfig() }))
            } else {
                ProxyNotice(
                    symbol: "doc.text",
                    title: "Configuration is managed externally",
                    detail: "Subscriptions changed, but this config is not built by "
                        + "JiejieBox, so it cannot be rebuilt here. Edit the file "
                        + "directly to apply the change.",
                    tone: .warning,
                    action: ("Open Config", { model.revealConfig() }))
            }

        case .noGroups:
            ProxyNotice(
                symbol: "square.stack.3d.up.slash",
                title: "No selector groups",
                detail: model.subscriptions.isEmpty
                    ? "No proxies yet. Add a subscription first."
                    : "The current configuration defines no selector groups.",
                tone: .neutral,
                action: model.subscriptions.isEmpty
                    ? ("Open Subscriptions", { model.path.append(.subscriptions) })
                    : (model.configRebuildable
                        ? ("Reload Config", { reloadConfig() })
                        : ("Open Config", { model.revealConfig() })))

        case .noGroupSelected:
            ProxyNotice(
                symbol: "square.stack.3d.up",
                title: "No group selected",
                detail: "Choose a selector group above.",
                tone: .neutral)

        case .empty:
            ProxyNotice(
                symbol: "tray",
                title: "This group has no nodes",
                detail: model.subscriptions.isEmpty
                    ? "Add or update a subscription, then reload the configuration."
                    : "Update the subscriptions to fetch the current node list.",
                tone: .neutral,
                action: model.subscriptions.isEmpty
                    ? ("Open Subscriptions", { model.path.append(.subscriptions) })
                    : ("Update Subscriptions", { updateAndReload() }))

        case .ready:
            if model.filteredProxies.isEmpty {
                ProxyNotice(
                    symbol: "magnifyingglass",
                    title: "No matching nodes",
                    detail: "Nothing matches “\(model.proxySearch)”.",
                    tone: .neutral,
                    action: ("Clear Search", { model.proxySearch = "" }))
            } else {
                nodeList
            }
        }
    }

    /// Reload the configuration, then re-read the groups so the list reflects
    /// what was rebuilt.
    ///
    /// Named rather than inlined: a multi-statement `Task { }` inside a ternary
    /// makes `Task.init` ambiguous on newer Swift toolchains, and the compiler
    /// reports it as an unhelpful "ambiguous use of init(name:priority:operation:)".
    private func reloadConfig() {
        Task { await model.reloadAndReloadGroups() }
    }

    /// Refresh subscriptions, then rebuild, then re-read the groups.
    private func updateAndReload() {
        Task { await model.updateSubscriptionsAndReload() }
    }

    private var loadingRow: some View {
        HStack(spacing: 8) {
            ProgressView().controlSize(.small)
            Text("Loading nodes…")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// The page's single vertical scroll owner.
    ///
    /// The frame is bounded by the scaffold (maxHeight: .infinity inside a
    /// fixed-size panel), so the list scrolls rather than growing the window.
    /// `basedOnSize` means a short list does not bounce.
    private var nodeList: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: Metrics.sectionSpacing) {
                ForEach(model.filteredProxies) { node in
                    nodeRow(node)
                }
            }
            .padding(.bottom, 4)
        }
        .scrollBounceBehavior(.basedOnSize)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: - Node row

    /// One node, as two independent actions.
    ///
    /// Selecting the node and measuring its latency are SIBLINGS, never a button
    /// inside a button: nesting them made the latency target unreliable and
    /// could switch the proxy when the user only asked to measure it. Select
    /// gets the larger share because it is the common action; the latency
    /// target is still a full-height region, not a small glyph.
    private func nodeRow(_ node: ProxyNode) -> some View {
        let selected = node.selected
        let switching = model.pending == .switchingProxy(node.name)
        let testing = model.pending == .testingProxy(node.name)

        // Availability is decided HERE, not left to the model's withPending
        // guard. A busy row disables BOTH of its actions — half a row staying
        // live mid-operation is inconsistent and invites a click that can only
        // be rejected with "another operation is running".
        //
        // Across rows the rules differ by operation: switches are serialised
        // (two would race for the same selection), while measurements are
        // independent and stay usable unless the whole group is being tested.
        //
        // `withPending` remains the second line of defence for a rapid click
        // that slips through; it is no longer the only thing preventing one.
        let rowBusy = switching || testing
        let selectDisabled = rowBusy || model.proxyGroupTestInFlight || model.proxySwitchInFlight
        let testDisabled = rowBusy || model.proxyGroupTestInFlight

        return ActionRow(actions: [
            RowAction(
                id: "select-\(node.id)",
                title: node.label,
                subtitle: node.type,
                value: nil,
                isPending: switching,
                isDisabled: selectDisabled,
                leading: AnyView(
                    Image(systemName: selected ? "checkmark.circle.fill" : "circle")
                        .font(.system(size: 12))
                        .foregroundStyle(selected ? AnyShapeStyle(.tint) : AnyShapeStyle(.tertiary))
                        .frame(width: 16)
                ),
                weight: 3,
                help: switching
                    ? "Applying…"
                    : (selected ? "Currently in use." : "Use this node."),
                action: { Task { await model.switchProxy(node) } }
            ),
            RowAction(
                id: "test-\(node.id)",
                title: "",
                value: node.delayLabel,
                valueColor: delayColor(node),
                isPending: testing,
                isDisabled: testDisabled,
                weight: 1,
                help: node.isMeasured
                    ? "Measure this node again."
                    : "Measure this node's latency.",
                action: { Task { await model.testProxy(node) } }
            ),
        ])
    }

    private func delayColor(_ node: ProxyNode) -> Color {
        if !node.isMeasured { return .secondary }
        if node.isFast { return .green }
        if node.isSlow { return .orange }
        return .primary
    }
}

/// A state explanation with an optional action.
///
/// Used instead of a bare sentence so every non-content state answers the same
/// three questions: what is wrong, why, and what can I do about it.
struct ProxyNotice: View {
    enum Tone { case neutral, warning, error }

    let symbol: String
    let title: String
    let detail: String
    var tone: Tone = .neutral
    /// Label and action for the way forward, when there is one.
    var action: (String, () -> Void)?

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: symbol)
                    .font(.system(size: 12))
                    .foregroundStyle(tint)
                Text(title)
                    .font(.callout.weight(.medium))
            }
            Text(detail)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if let action {
                Button(action: action.1) {
                    Text(action.0)
                }
                .controlSize(.small)
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var tint: Color {
        switch tone {
        case .neutral: return .secondary
        case .warning: return .orange
        case .error: return .red
        }
    }
}
