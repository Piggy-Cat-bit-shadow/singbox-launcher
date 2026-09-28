// HomeView — the menu-bar panel.
//
// Design intent: answer the seven questions a menu-bar user actually has,
// top to bottom, without a dashboard.
//
//   1. is the VPN running?          → status line
//   2. how fast is it going?        → speed readout
//   3. which engine / mode?         → Runtime section
//   4. which group and node?        → Network section
//   5. what do I do now?            → the Start/Stop control
//   6. where is everything else?    → More
//   7. how do I quit?               → the header, always
//
// The previous revision used a full-width prominent button, which outweighed
// every other element on the panel and read as a web form rather than a macOS
// utility. The primary control is now a compact button sitting next to the
// status it acts on, so the visual weight matches how often it is used.

import SwiftUI

struct HomeView: View {
    let model: AppModel
    @Environment(\.localization) private var language
    /// Gates the ownership handover behind an explicit confirmation.
    ///
    /// Not a convenience: adopting means the launcher may from then on REBUILD
    /// AND OVERWRITE config.json, so it must be a deliberate act that names the
    /// consequence, never a side effect of dismissing a warning.
    ///
    /// Owned by the MODEL, not by a stored property here: this toolchain cannot
    /// compile `@State` (Swift 6.4 command-line, SwiftUI macros unavailable), and
    /// a `private let` holder on a View struct is replaced whenever the parent
    /// body runs — which would dismiss a confirmation dialog the user is reading.
    private var adoptPrompt: TextDraft {
        model.drafts.draft(DraftStore.homeAdoptConfig)
    }

    var body: some View {
        PanelScaffold(model: model, title: "JiejieBox", showsFeedback: false) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                statusCard
                banners
                runtimeSection
                networkSection
                navigationSection
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
        }
        .confirmationDialog(L.adoptConfig.tr(language),
                            isPresented: Binding(
                                get: { adoptPrompt.text == DraftStore.confirmWord },
                                set: { if !$0 { adoptPrompt.text = "" } }),
                            titleVisibility: .visible) {
            Button(L.adoptConfig.tr(language)) {
                adoptPrompt.text = ""
                Task { await model.adoptConfig() }
            }
            Button(L.cancel.tr(language), role: .cancel) { adoptPrompt.text = "" }
        } message: {
            // States the consequence in full: the config may be REBUILT and
            // OVERWRITTEN from now on. An ownership handover that does not say
            // so is not consent.
            Text(L.adoptConfigConfirm.tr(language))
        }
    }

    /// The banner text for the current ownership.
    ///
    /// UNKNOWN gets its own sentence, deliberately NOT the external one: it says
    /// what is actually known ("cannot confirm that JiejieBox produced this")
    /// instead of asserting an owner we have no evidence for.
    private var ownershipMessage: String {
        switch model.configOwnership {
        case .managed: return L.configChanged.tr(language)
        case .unknown: return L.configChangedUnknown.tr(language)
        case .external: return L.configChangedExternal.tr(language)
        }
    }

    /// The localized reason the last start failed, or nil when it did not.
    ///
    /// Keyed off the backend's STABLE error code rather than off the message
    /// text, so the explanation survives translation and can be improved without
    /// touching the backend.
    private var startFailure: String? {
        guard let code = model.core?.error_code, !code.isEmpty else { return nil }
        switch code {
        case "config_rebuild_failed": return L.startFailedConfigRebuild.tr(language)
        case "core_start_failed": return L.startFailedSpawn.tr(language)
        case "daemon_unreachable": return L.startFailedDaemonUnreachable.tr(language)
        case "daemon_apply_failed": return L.startFailedDaemonApply.tr(language)
        case "config_check_failed": return L.startFailedConfigCheck.tr(language)
        case "clash_api_port_in_use": return L.startFailedPortInUse.tr(language)
        case "cancelled": return L.startFailedCancelled.tr(language)
        // Codes added with the unified lifecycle error store. Each one maps to a
        // sentence that says what to DO, not just that something failed: an
        // occupied port and a missing protected copy need different actions from
        // the user, and collapsing both into "start failed" would leave them
        // guessing.
        case "privileged_copy_unavailable": return L.startFailedPrivilegedCopy.tr(language)
        case "permission_denied": return L.startFailedPermission.tr(language)
        case "authorization_timeout": return L.startFailedAuthTimeout.tr(language)
        case "core_fast_exit": return L.startFailedFastExit.tr(language)
        case "restart_exhausted": return L.startFailedRestartExhausted.tr(language)
        case "stop_failed": return L.stopFailed.tr(language)
        default:
            // An unrecognised code still means a failure happened, and `error`
            // state is never shown silently.
            return L.startFailed.tr(language)
        }
    }

    /// A reassurance appended to a CONFIG failure, or nil for other failures.
    ///
    /// The backend sets `config_error` only for failures from building or
    /// activating the config — the cases where config.json was deliberately NOT
    /// replaced. Saying "your previous working config is still in use" after a
    /// spawn failure would be untrue, so it is gated on that field.
    private var configFailureNote: String? {
        guard let note = model.core?.config_error, !note.isEmpty else { return nil }
        return " — " + L.configWasNotReplaced.tr(language)
    }

    // MARK: - Status and the primary control
    //
    // Status and its action share one row: the button belongs to the state it
    // changes, and pairing them removes the need for a giant call-to-action.

    private var statusCard: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .center, spacing: 10) {
                VStack(alignment: .leading, spacing: 2) {
                    StatusLine(state: model.core?.state, error: model.core?.error_message,
                               language: language)
                    versionRow
                }

                Spacer(minLength: 8)

                primaryButton
            }

            if model.core?.state == .running, let rate = model.traffic {
                Divider().padding(.vertical, 2)
                speedReadout(rate)
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 6)
    }

    /// The sing-box version line, which doubles as the core-replacement entry
    /// point.
    ///
    /// The version is where a user looks to answer "which core am I running?",
    /// so the action that changes it belongs on the same line rather than
    /// buried in a menu. Three states, deliberately distinct:
    ///
    ///   * import available and a core is installed — a button, so the row is
    ///     reachable and obviously interactive;
    ///   * import available and no core — the same button, phrased as loading
    ///     one, because that is the only way out of the missing-core state;
    ///   * import unavailable — plain text. Offering an action the backend
    ///     cannot perform produces a button whose only outcome is an error.
    @ViewBuilder
    private var versionRow: some View {
        if model.coreImportAvailable {
            Button {
                loadCore()
            } label: {
                HStack(spacing: 4) {
                    if model.pending == .importingCore {
                        ProgressView().controlSize(.mini)
                        Text(L.installingCore.tr(language))
                    } else if let version = installedVersion {
                        Text(version)
                            .lineLimit(1)
                            .truncationMode(.middle)
                        Image(systemName: "arrow.triangle.2.circlepath")
                            .font(Typography.microGlyph)
                    } else {
                        Text(L.loadCore.tr(language))
                    }
                }
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            // The BACKEND's precondition, not merely "nothing is pending".
            //
            // `ImportCore` refuses unless the core is settled stopped, so this
            // row let the user open a file chooser, pick a binary, wait for the
            // panel, and only then be told to stop the VPN first. The policy is
            // shared with every other core control so the reason and the
            // disabled state cannot disagree.
            .disabled(!corePolicy.canImportCore)
            .help(corePolicy.canImportCore
                  ? (installedVersion == nil
                        ? L.loadCoreHelp.tr(language)
                        : L.replaceCoreHelp.tr(language))
                  : (model.core?.state == .running
                        ? L.stopVPNBeforeReplacingCore.tr(language)
                        : (corePolicy.reason ?? "")))
        } else if let version = installedVersion {
            Text(version)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
                .help(version)
        }
    }

    /// Installed core version, or nil when none is known.
    private var installedVersion: String? {
        guard let version = model.core?.core_version, !version.isEmpty else { return nil }
        return version
    }

    /// Ask for a core binary and hand the path to the backend.
    ///
    /// A cancelled panel is not a failure, so nothing is reported; the backend
    /// speaks for every rejected candidate.
    private func loadCore() {
        guard let path = FilePicker.chooseCoreBinary() else { return }
        Task { await model.importCoreFile(path: path) }
    }

    private var primaryButton: some View {
        Button {
            // A non-recoverable failure is not retried: the label says "Review
            // details", and pressing it must do that rather than firing the
            // start that the backend just said cannot work.
            // The TAP and the LABEL read the same decision. Deciding them
            // separately is how a button ends up reading "Review Details" while
            // still issuing a retry — the two would drift the moment either rule
            // changed.
            switch primaryAction {
            case .reviewDetails:
                model.path.append(.coreDetails)
            case .start, .stop, .retry:
                Task { await model.toggleCore() }
            case .starting, .stopping:
                // Nothing to do: the command is already in flight, and the
                // disabled state above is what prevents a second one.
                break
            }
        } label: {
            if primaryIsBusy {
                HStack(spacing: 6) {
                    ProgressView().controlSize(.small)
                    Text(primaryTitle)
                }
                .frame(minWidth: 78)
            } else {
                Text(primaryTitle)
                    .frame(minWidth: 78)
            }
        }
        .controlSize(.large)
        .buttonStyle(.borderedProminent)
        // Prominent only while it is the thing to do; once connected the button
        // is a plain-bordered stop control, so a running VPN does not shout.
        .tint(model.core?.state == .running ? nil : .accentColor)
        .disabled(!canAct)
        .help(primaryHelp)
    }

    /// The button's label, reflecting BOTH the reported state and a command in
    /// flight. The pending case matters because `start_core` returns before the
    /// core reaches `starting`, so relying on the state alone left a window
    /// where the button still read L.start.tr(language) after being clicked.
    private var primaryAction: HomePrimaryAction {
        // The decision lives in ActionPolicy so the Go suite EXECUTES it. It is
        // the rule that decides whether a user is offered a retry that cannot
        // succeed, and whether a click is acknowledged before the backend catches
        // up — neither of which a source-text check can establish.
        homePrimaryAction(state: model.core?.state,
                          pendingStart: model.pending == .startingCore,
                          pendingStop: model.pending == .stoppingCore,
                          recoverable: model.core?.recoverable)
    }

    private var primaryTitle: String {
        switch primaryAction {
        case .start: return L.start.tr(language)
        case .stop: return L.stop.tr(language)
        case .starting: return L.starting.tr(language)
        case .stopping: return L.stopping.tr(language)
        case .retry: return L.retry.tr(language)
        case .reviewDetails: return L.reviewDetails.tr(language)
        }
    }

    /// True while the primary button should show a spinner.
    private var primaryIsBusy: Bool {
        model.pending == .startingCore
            || model.pending == .stoppingCore
            || model.core?.state.isTransitioning == true
    }

    /// Disabled only during a transition or when there is nothing to start, with
    /// the reason in the help text rather than an unexplained dead control.
    /// The shared core policy, for every control on this screen.
    private var corePolicy: CoreActionPolicy {
        model.coreActionPolicy(language: language)
    }

    /// Whether the primary button may act.
    ///
    /// Delegates to the policy rather than re-deriving: the button's enabled
    /// state, its label and its tooltip are three renderings of one decision.
    private var canAct: Bool {
        guard let state = model.core?.state else { return false }
        let policy = corePolicy
        // The control is a toggle: which half applies depends on the state the
        // label is describing.
        return state == .running ? policy.canStop : policy.canStart
    }

    private var primaryHelp: String {
        guard let core = model.core else { return "" }
        if model.coreOperationBusy { return L.waitForOperation.tr(language) }
        if core.state != .running && !core.binary_exists {
            return L.coreNotFound.tr(language)
        }
        return core.state == .running
            ? L.stopTheCore.tr(language)
            : L.startTheCore.tr(language)
    }

    /// Up/down speed, monospaced so the numbers do not jitter the layout.
    private func speedReadout(_ rate: TrafficRate) -> some View {
        HStack(spacing: 16) {
            speedItem(symbol: "arrow.down", value: ByteFormat.rate(rate.down),
                      total: rate.total_down, label: L.downloaded.tr(language))
            speedItem(symbol: "arrow.up", value: ByteFormat.rate(rate.up),
                      total: rate.total_up, label: L.uploaded.tr(language))
            Spacer(minLength: 0)
        }
    }

    private func speedItem(symbol: String, value: String, total: Int64, label: String) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Label(value, systemImage: symbol)
                .font(Typography.numeric)
            Text("\(ByteFormat.size(total)) total")
                .font(Typography.rowSubtitle)
                .foregroundStyle(.tertiary)
        }
        .help("\(label) since connecting")
    }

    // MARK: - Banners
    //
    // Persistent conditions and transient toasts are rendered SEPARATELY rather
    // than as one else-if chain. Chained, a success message such as
    // "Subscriptions updated" would suppress "the core binary is missing": the
    // user would lose a standing warning because an unrelated action succeeded.
    //
    // Order within the persistent group is by severity — the first shown is the
    // one that blocks everything else.

    @ViewBuilder
    private var banners: some View {
        persistentBanners
        transientBanner
    }

    /// Standing conditions that last until they are actually resolved.
    @ViewBuilder
    private var persistentBanners: some View {
        if case .failed(let message) = model.connection {
            Banner(kind: .error, message: message) {
                Button(L.restart.tr(language)) { Task { await model.restart() } }
                    // The banner's Restart had NO condition at all, so it stayed
                    // clickable through the teardown it had just started — the
                    // double-click window the model's single-flight now closes,
                    // but the control should not invite it either.
                    .disabled(model.backendRestartInFlight || model.connection == .connecting)
                    .controlSize(.small)
            }
        } else if model.coreMissing {
            // With no core at all, choosing one is the only way forward, so the
            // banner leads with it. Revealing the folder stays available as the
            // second option for someone who wants to place the binary by hand.
            Banner(kind: .error, message: L.coreNotFound.tr(language)) {
                if model.coreImportAvailable {
                    Button(L.loadCore.tr(language)) { loadCore() }
                        .controlSize(.small)
                        .disabled(model.pending != nil)
                }
                Button(L.revealFolder.tr(language)) { model.revealConfigFolder() }
                    .controlSize(.small)
            }
        } else if model.configMissing {
            Banner(kind: .warning, message: L.noConfigYet.tr(language)) {
                Button(L.subscriptions.tr(language)) { model.path.append(.subscriptions) }
                    .controlSize(.small)
            }
        } else if let failure = startFailure {
            // The reason a start failed, next to the control that failed.
            //
            // Before this, a failed start produced NOTHING on screen: the button
            // returned to "Start" and the explanation existed only in a log
            // file. The forbidden sequence — press Start, immediately see Start
            // again, with no explanation — was exactly this banner's absence.
            //
            // The sentence is chosen by the backend's stable code and translated
            // here; the raw detail is attached as a tooltip rather than printed,
            // because it is untranslated and names internal functions.
            // When the failure came from the CONFIG pipeline, the user's VPN
            // story is not "it broke" but "you are still on the last working
            // config". That is the fact that decides whether they need to act
            // now, so it is said explicitly instead of being left to inference.
            // Only shown for config failures: claiming it after a spawn failure
            // would be false reassurance.
            Banner(kind: .warning, message: failure + (configFailureNote ?? "")) {
                Button(L.openLogs.tr(language)) { model.openLogs() }
                    .controlSize(.small)
            }
            .help(model.core?.error_detail ?? "")
        } else if model.core?.config_stale == true {
            // Three distinct situations, three distinct messages.
            //
            // The message keys off OWNERSHIP, not off `configRebuildable`.
            // Reading "cannot rebuild" as "somebody else owns it" is what told
            // users that a config their own copy of JiejieBox had written was
            // managed by another tool: such a config predates provenance
            // markers, so it is UNKNOWN — unproven, not foreign.
            //
            // UNKNOWN is never silently repaired either. It offers the file and,
            // only behind an explicit confirmation that says what will happen,
            // the option to hand ownership over.
            Banner(kind: .warning, message: ownershipMessage) {
                switch model.configOwnership {
                case .managed:
                    Button(L.reload.tr(language)) {
                        Task {
                            await model.reloadConfig()
                            await model.refreshCoreState()
                        }
                    }
                    .controlSize(.small)
                case .unknown:
                    Button(L.openConfig.tr(language)) { model.revealConfig() }
                        .controlSize(.small)
                    Button(L.adoptConfig.tr(language)) {
                        adoptPrompt.text = DraftStore.confirmWord
                    }
                    .controlSize(.small)
                case .external:
                    Button(L.openConfig.tr(language)) { model.revealConfig() }
                        .controlSize(.small)
                }
            }
        }

        // A failure from the last action. Cleared by the user or by the next
        // success — never on a timer, because an error the user never read is
        // an error that did not happen.
        if let error = model.lastError, !error.isEmpty {
            errorBanner(error)
        }
    }

    /// A success message that clears itself, shown independently of the
    /// persistent conditions above.
    @ViewBuilder
    private var transientBanner: some View {
        if let status = model.transientStatus, !status.isEmpty {
            Banner(kind: .info, message: status) {
                Button {
                    model.clearTransientStatus()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                }
                .buttonStyle(.plain)
                // An icon-only control has NO accessible name of its own: read
                // aloud it is an unnamed button, so a VoiceOver user cannot tell
                // what dismissing it does — or that it is dismissible at all.
                .help(L.dismiss.tr(language))
                .accessibilityLabel(L.dismiss.tr(language))
            }
        }
    }

    /// A failure the user must see; dismissible so it cannot trap the panel.
    private func errorBanner(_ message: String) -> some View {
        Banner(kind: .error, message: message) {
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

    // MARK: - Runtime

    private var runtimeSection: some View {
        MenuSection(L.core.tr(language)) {
            MenuRow(L.coreDetails.tr(language), systemImage: "info.circle",
                    value: model.core?.state.label(language) ?? "—",
                    showsChevron: true) {
                model.path.append(.coreDetails)
            }
            MenuRow(L.coreMode.tr(language), systemImage: "gearshape",
                    value: coreModeValue,
                    showsChevron: true) {
                model.path.append(.coreMode)
            }
        }
    }

    /// Which engine is in use, as a localized word.
    ///
    /// The backend reports an identifier ("classic"/"daemon"), never prose, so
    /// the mapping lives here rather than comparing against a rendered English
    /// label — which would break the moment the interface is not in English.
    private var coreModeValue: String {
        model.coreModeLabel(language)
    }

    // MARK: - Network

    private var networkSection: some View {
        MenuSection(L.proxies.tr(language)) {
            // Always navigable, even with the core stopped. ProxiesView
            // already explains "start the core to list and switch proxies", so
            // disabling the entry hid the very explanation the user needs — the
            // same navigation-vs-mutation mistake as the daemon row: not being
            // able to CHANGE something is not a reason to hide its STATUS.
            MenuRow(L.proxies.tr(language), systemImage: "arrow.triangle.branch",
                    value: proxySummary,
                    showsChevron: true) {
                model.path.append(.proxies)
            }
            .help(model.core?.state == .running
                  ? L.chooseProxyGroup.tr(language)
                  : L.startCoreToChoose.tr(language))
        }
    }

    /// The node in use.
    ///
    /// Read from the snapshot's summary, NOT from `model.proxies`: that list is
    /// only loaded once the Proxies screen has been opened, so deriving this
    /// from it left Home showing "Choose…" while a proxy was in fact selected.
    private var proxySummary: String {
        guard model.core?.state == .running else { return "—" }
        guard let summary = model.proxySummary, summary.hasSelection else {
            return L.choose.tr(language)
        }
        if let delay = summary.delayLabel {
            return "\(summary.label) · \(delay)"
        }
        return summary.label
    }

    // MARK: - Navigation

    private var navigationSection: some View {
        MenuSection(L.manage.tr(language)) {
            MenuRow(L.subscriptions.tr(language), systemImage: "arrow.down.circle",
                    value: subscriptionSummary,
                    showsChevron: true) {
                model.path.append(.subscriptions)
            }
            MenuRow(L.more.tr(language), systemImage: "ellipsis.circle", showsChevron: true) {
                model.path.append(.more)
            }
        }
    }

    private var subscriptionSummary: String {
        let count = model.subscriptions.count
        return count == 0 ? L.none.tr(language) : "\(count)"
    }
}

// MARK: - Shared pieces

/// The status dot and label.
struct StatusLine: View {
    let state: CoreState?
    var error: String?
    /// Injected rather than read from the environment: this view is also used
    /// from the Proxies screen, and passing the language keeps its output
    /// deterministic and testable.
    var language: Localization = .en

    var body: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(color)
                .frame(width: 8, height: 8)
            Text(label)
                .font(Typography.statusPrimary)
                .foregroundStyle(.primary)
                .lineLimit(1)
        }
        .help(error ?? "")
    }

    private var label: String {
        guard let state else { return L.connecting.tr(language) }
        return state.label(language)
    }

    private var color: Color {
        switch state {
        case .running: return .green
        case .starting, .stopping: return .orange
        case .error: return .red
        case .stopped, .none: return .secondary
        }
    }
}

/// An inline message with an optional action.
///
/// Rendered as a row rather than a coloured card: the panel is small, and a
/// full-bleed tinted box competes with the content it is meant to annotate.
struct Banner<Action: View>: View {
    enum Kind { case info, warning, error }

    let kind: Kind
    let message: String
    @ViewBuilder var action: () -> Action

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Image(systemName: symbol)
                .foregroundStyle(tint)
                .font(Typography.badge)
            Text(message)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 8)
            action()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 5)
    }

    private var symbol: String {
        switch kind {
        case .info: return "info.circle"
        case .warning: return "exclamationmark.triangle"
        case .error: return "xmark.octagon"
        }
    }

    private var tint: Color {
        switch kind {
        case .info: return .secondary
        case .warning: return .orange
        case .error: return .red
        }
    }
}
