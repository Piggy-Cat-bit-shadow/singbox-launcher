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
    @Environment(\.localization) private var language

    var body: some View {
        // scrollsContent: false — this page's node list is the scroll owner.
        // The scaffold would otherwise wrap it in a second vertical ScrollView.
        PanelScaffold(model: model, title: L.proxies.tr(language),
                      onBack: { model.goBack() },
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
                    // The switch is offered one at a time. The model already
                    // refuses overlapping reads by generation, but a control that
                    // is clickable while its own request is outstanding invites
                    // exactly the rapid A-B-C sequence that produces the
                    // out-of-order replies the generation exists to reject.

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
                    .font(Typography.badge)
                    .foregroundStyle(.secondary)
                Text(currentGroupLabel)
                    .font(Typography.rowTitle.weight(.medium))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Image(systemName: "chevron.up.chevron.down")
                    .font(Typography.inlineGlyph)
                    .foregroundStyle(.secondary)
            }
        }
        .menuStyle(.borderlessButton)
        .fixedSize()
        // Disabled while a node list is loading OR another operation holds the
        // model. Without this the picker stayed live during its own request, so
        // a user could queue A, B and C faster than the backend answered — the
        // exact sequence whose replies can arrive out of order.
        .disabled(model.proxiesLoading || model.pending != nil)
        .help(pickerHelp)
    }

    /// Explains a disabled picker instead of leaving a grey control unexplained.
    private var pickerHelp: String {
        if model.proxiesLoading { return L.loadingNodes.tr(language) }
        if model.pending != nil { return L.anotherOperationRunning.tr(language) }
        if model.groups.count > 1 {
            return "\(L.switchGroupHelp.tr(language)) \(model.groups.count) \(L.groupCountHelp.tr(language))"
        }
        return L.activeSelectorGroup.tr(language)
    }

    /// A real button with its own hit area, not a floating label. Disabled with
    /// a reason in the tooltip when there is nothing to test or the backend
    /// cannot run a test.
    private var testAllButton: some View {
        Button {
            Task { await model.testGroup() }
        } label: {
            HStack(spacing: 5) {
                if let progress = model.groupTest.progress {
                    ProgressView().controlSize(.mini)
                    // "Testing 12/36": the count is the whole point of streaming
                    // progress — a bare spinner gives no sense of whether a
                    // 200-node group is nearly done or barely started.
                    Text(L.testingProgress.tr(language, progress.completed, progress.total))
                        .font(Typography.rowValue.weight(.medium))
                } else {
                    Image(systemName: "bolt.horizontal")
                        .font(Typography.rowSubtitle)
                    Text(L.testAll.tr(language))
                        .font(Typography.rowValue.weight(.medium))
                }
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
        // Engine capability first: on an engine that cannot measure, the button
        // must be disabled and EXPLAINED rather than clickable into a failure.
        guard model.proxyActions.can_test_group else { return false }
        guard model.proxyListState == .ready else { return false }
        return model.pending == nil && !model.groupTest.isRunning
    }

    private var testAllHelp: String {
        if !model.proxyActions.can_test_group {
            return L.latencyUnsupported.tr(language)
        }
        switch model.proxyListState {
        case .coreStopped: return L.coreStoppedTest.tr(language)
        case .backendUnavailable: return L.backendUnavailableShort.tr(language)
        case .empty: return L.noNodesToTest.tr(language)
        case .ready:
            return model.pending == nil
                ? L.measureAllHelp.tr(language)
                : L.anotherOpRunning.tr(language)
        default: return L.nothingToTest.tr(language)
        }
    }

    private func groupSelectionLabel(_ group: ProxyGroup) -> String {
        guard let selected = group.selected_display ?? group.selected, !selected.isEmpty else {
            return group.label
        }
        return "\(group.label) — \(selected)"
    }

    private var currentGroupLabel: String {
        // While a switch is in flight the header names the group being LOADED,
        // not the one still on screen. Otherwise the label says A, then jumps to
        // B when the reply lands, with no indication anything was happening —
        // and a user who clicks twice has no feedback either way.
        let target = model.pendingSelectedGroup ?? model.selectedGroup
        let group = model.groups.first { $0.name == target }
        let base = group?.label
            ?? (model.selectedGroup.isEmpty ? L.noGroup.tr(language) : model.selectedGroup)
        guard let selected = group?.selected_display ?? group?.selected, !selected.isEmpty else {
            return base
        }
        return "\(base) — \(selected)"
    }

    // MARK: - Search

    private var searchField: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
            TextField(L.searchNodes.tr(language), text: Binding(get: { model.proxySearch },
                                                    set: { model.proxySearch = $0 }))
                .textFieldStyle(.plain)
                .font(Typography.rowValue)
            // Clearing matters on a filtered list: without it the only way back
            // to the full list is selecting and deleting the text by hand.
            if !model.proxySearch.isEmpty {
                Button {
                    model.proxySearch = ""
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help(L.clearSearch.tr(language))
            }
            if !model.proxies.isEmpty {
                Text(countLabel)
                    .font(Typography.rowSubtitle)
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
            BackendDownView(model: model)

        case .loading, .idle:
            loadingRow

        case .coreStopped:
            ProxyNotice(
                symbol: "power",
                title: L.coreNotRunning.tr(language),
                detail: L.coreNotRunningDetail.tr(language),
                tone: .neutral)

        case .unsupportedByEngine:
            // An explanation, NOT an error: no user action changes an engine
            // capability, so there is deliberately no Retry. Offering one would
            // promise a fix that cannot happen.
            ProxyNotice(
                symbol: "info.circle",
                title: L.proxiesUnsupportedTitle.tr(language),
                detail: model.proxiesUnsupportedReason == "daemon_no_group_rpc"
                    ? L.proxiesUnsupportedDaemon.tr(language)
                    : L.proxiesUnsupportedGeneric.tr(language),
                tone: .neutral,
                action: (L.coreMode.tr(language), { model.path.append(.coreMode) }))

        case .failed:
            ProxyNotice(
                symbol: "exclamationmark.triangle",
                title: L.couldNotLoadProxies.tr(language),
                detail: model.proxyError ?? L.backendDidNotAnswer.tr(language),
                tone: .error,
                action: (L.tryAgain.tr(language), { Task { await model.loadGroups() } }),
                actionEnabled: !model.proxiesLoading,
                actionIsPending: model.proxiesLoading)

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
                    action: (L.reloadConfigAction.tr(language), { reloadConfig() }),
                    actionEnabled: model.canReloadConfig,
                    actionIsPending: model.pending == .reloadingConfig)
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
                title: L.noSelectorGroups.tr(language),
                detail: model.subscriptions.isEmpty
                    ? L.noProxiesYet.tr(language)
                    : L.noSelectorGroupsDetail.tr(language),
                tone: .neutral,
                action: model.subscriptions.isEmpty
                    ? (L.openSubscriptions.tr(language), { model.path.append(.subscriptions) })
                    : (model.configRebuildable
                        ? (L.reloadConfigAction.tr(language), { reloadConfig() })
                        : (L.openConfigPlain.tr(language), { model.revealConfig() })))

        case .noGroupSelected:
            ProxyNotice(
                symbol: "square.stack.3d.up",
                title: L.noGroupSelected.tr(language),
                detail: L.chooseGroupAbove.tr(language),
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
                    ? (L.openSubscriptions.tr(language), { model.path.append(.subscriptions) })
                    : (L.updateSubscriptionsAction.tr(language), { updateAndReload() }),
                actionEnabled: model.subscriptions.isEmpty || model.canUpdateAllSubscriptions,
                actionIsPending: model.pending == .updatingSubscriptions,
                actionDisabledReason: L.noRefreshableSubscriptions.tr(language))

        case .ready:
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                // STALENESS IS SHOWN IN THE READY STATE TOO.
                //
                // This is the case that used to hide it. With nodes cached, the
                // list looked completely healthy while the running config no
                // longer matched the sources — so a node the user had just
                // deleted still appeared switchable, with nothing on screen
                // saying the list was out of date. The banner sits ABOVE the
                // list: the nodes stay usable (they are the best information
                // available until a reload) but they are labelled for what they
                // are.
                if model.proxyListIsStale {
                    staleBanner
                }
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
    }

    /// The out-of-date notice shown above a cached node list.
    ///
    /// Its action depends on config ownership for the same reason the
    /// `.configStale` state does: offering Reload to a user whose config is
    /// managed by another tool leads only to a refusal.
    @ViewBuilder
    private var staleBanner: some View {
        if model.configRebuildable {
            ProxyNotice(
                symbol: "arrow.triangle.2.circlepath",
                title: L.nodeListOutOfDate.tr(language),
                detail: L.nodeListOutOfDateDetail.tr(language),
                tone: .warning,
                action: (L.reloadConfigAction.tr(language), { reloadConfig() }),
                actionEnabled: model.canReloadConfig,
                actionIsPending: model.pending == .reloadingConfig)
        } else {
            ProxyNotice(
                symbol: "doc.text",
                title: L.configManagedExternally.tr(language),
                detail: L.configManagedExternallyDetail.tr(language),
                tone: .warning,
                action: (L.openConfigPlain.tr(language), { model.revealConfig() }))
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
            Text(L.loadingNodes.tr(language))
                .font(Typography.rowValue)
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

        // AVAILABILITY COMES FROM ONE RULE, SHARED WITH THE MODEL'S GUARD.
        //
        // `withPending` admits ONE operation at a time, so a second single-node
        // test is refused by the model — but the row's own disable rule did not
        // mention that state, leaving every OTHER node's Test control looking
        // live. Clicking one produced "another operation is running", an error
        // this screen deliberately does not display, so the user saw a control
        // that did nothing at all.
        //
        // `proxyRowPolicy` states the product rule once — measurements are
        // SERIALISED because they share the core's delay endpoint and measurement
        // table — and both the guard and every control read it. Putting it in
        // ActionPolicy also means the Go suite EXECUTES it, including the
        // invariant that no row offers what the model would refuse.
        let rowPolicy = proxyRowPolicy(
            rowID: node.id,
            singleTestInFlightFor: model.singleTestInFlightNodeID,
            switchInFlightFor: model.switchInFlightNodeID,
            groupTestRunning: model.proxyGroupTestInFlight,
            listLoading: model.proxiesLoading)
        let testDisabled = !rowPolicy.canTest
        let selectDisabled = !rowPolicy.canSelect

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
                        .font(Typography.badge)
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
                // A node that failed shows WHY it has no number ("timed out")
                // rather than "0 ms", which would read as the fastest node in
                // the list. The technical reason stays in the tooltip so the
                // list does not become a wall of transport errors.
                value: node.delayLabel(language),
                valueColor: delayColor(node),
                isPending: testing || groupTesting(node),
                isDisabled: testDisabled,
                weight: 1,
                help: testHelp(node),
                // The visual title is empty by design (the row shows the number),
                // so the spoken label has to be supplied separately or VoiceOver
                // announces nothing at all.
                accessibilityLabel: node.delayAccessibilityLabel(language),
                action: { Task { await model.testProxy(node) } }
            ),
        ])
    }

    /// True while this node's measurement is in flight during a group test.
    ///
    /// Only nodes actually occupying a worker show a spinner; the ones still
    /// queueing keep their previous value, which is what makes the streamed
    /// progress legible instead of every row spinning at once.
    private func groupTesting(_ node: ProxyNode) -> Bool {
        model.groupTest.progress?.inFlight.contains(node.name) ?? false
    }

    private func testHelp(_ node: ProxyNode) -> String {
        // The real reason, for a user who wants it. The row itself stays short.
        if let error = node.last_error, !error.isEmpty { return error }
        if node.didFailMeasurement { return L.measureAllHelp.tr(language) }
        return node.isMeasured
            ? L.measureAgainHelp.tr(language)
            : L.measureNodeHelp.tr(language)
    }

    private func delayColor(_ node: ProxyNode) -> Color {
        // A failure is never coloured as a good result, whatever the bucket.
        if node.didFailMeasurement { return .orange }
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
    /// Whether the action can run right now.
    ///
    /// A notice action is a real command (Reload Config, Update Subscriptions,
    /// Try Again) and is subject to the same one-operation-at-a-time rule as
    /// every other control. Nothing enforced that here, so while a reload or an
    /// update was in flight these buttons still looked live and the click was
    /// refused by the model — with the reason going to a banner this screen does
    /// not draw. The caller passes the model's own answer rather than each notice
    /// re-deriving it.
    var actionEnabled: Bool = true
    /// Shown while this notice's action is the operation in flight.
    var actionIsPending: Bool = false
    /// Why the action is unavailable, when it is.
    var actionDisabledReason: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: symbol)
                    .font(Typography.badge)
                    .foregroundStyle(tint)
                Text(title)
                    .font(Typography.rowTitle.weight(.medium))
            }
            Text(detail)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if let action {
                HStack(spacing: 6) {
                    Button(action: action.1) {
                        Text(action.0)
                    }
                    .controlSize(.small)
                    .disabled(!actionEnabled)
                    .help(actionEnabled ? "" : (actionDisabledReason ?? ""))
                    if actionIsPending {
                        ProgressView().controlSize(.small)
                    }
                }
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
