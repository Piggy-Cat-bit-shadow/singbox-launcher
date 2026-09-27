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

    var body: some View {
        PanelScaffold(model: model, title: "JiejieBox") {
            VStack(alignment: .leading, spacing: 10) {
                statusCard
                banners
                runtimeSection
                networkSection
                navigationSection
            }
            .padding(.vertical, 10)
        }
    }

    // MARK: - Status and the primary control
    //
    // Status and its action share one row: the button belongs to the state it
    // changes, and pairing them removes the need for a giant call-to-action.

    private var statusCard: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .center, spacing: 10) {
                VStack(alignment: .leading, spacing: 2) {
                    StatusLine(state: model.core?.state, error: model.core?.error_message)
                    if let version = model.core?.core_version, !version.isEmpty {
                        Text(version)
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .truncationMode(.middle)
                            .help(version)
                    }
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

    private var primaryButton: some View {
        Button {
            Task { await model.toggleCore() }
        } label: {
            if model.core?.state.isTransitioning == true {
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

    private var primaryTitle: String {
        guard let state = model.core?.state else { return "Start" }
        switch state {
        case .running: return "Stop"
        case .starting: return "Starting…"
        case .stopping: return "Stopping…"
        case .error: return "Retry"
        case .stopped: return "Start"
        }
    }

    /// Disabled only during a transition or when there is nothing to start, with
    /// the reason in the help text rather than an unexplained dead control.
    private var canAct: Bool {
        guard case .ready = model.connection else { return false }
        guard let core = model.core else { return false }
        // Shared policy: a command in flight anywhere (a proxy switch, a config
        // reload, a mode change) blocks starting and stopping too, so two core
        // commands can never overlap.
        if model.coreOperationBusy { return false }
        if core.state != .running && !core.binary_exists { return false }
        return true
    }

    private var primaryHelp: String {
        guard let core = model.core else { return "" }
        if model.coreOperationBusy { return "Please wait for the current operation to finish." }
        if core.state != .running && !core.binary_exists {
            return "The sing-box core binary was not found."
        }
        return core.state == .running ? "Stop the core" : "Start the core"
    }

    /// Up/down speed, monospaced so the numbers do not jitter the layout.
    private func speedReadout(_ rate: TrafficRate) -> some View {
        HStack(spacing: 16) {
            speedItem(symbol: "arrow.down", value: ByteFormat.rate(rate.down),
                      total: rate.total_down, label: "Downloaded")
            speedItem(symbol: "arrow.up", value: ByteFormat.rate(rate.up),
                      total: rate.total_up, label: "Uploaded")
            Spacer(minLength: 0)
        }
    }

    private func speedItem(symbol: String, value: String, total: Int64, label: String) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Label(value, systemImage: symbol)
                .font(.callout.monospacedDigit())
            Text("\(ByteFormat.size(total)) total")
                .font(.caption2)
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
                Button("Restart") { Task { await model.restart() } }
                    .controlSize(.small)
            }
        } else if model.coreMissing {
            Banner(kind: .error, message: "The sing-box core binary was not found.") {
                Button("Reveal Folder") { model.revealConfigFolder() }
                    .controlSize(.small)
            }
        } else if model.configMissing {
            Banner(kind: .warning, message: "No config.json yet. Add a subscription to build one.") {
                Button("Subscriptions") { model.path.append(.subscriptions) }
                    .controlSize(.small)
            }
        } else if model.core?.config_stale == true {
            Banner(kind: .warning, message: "The configuration has changed since it was built.") {
                Button("Reload") {
                    Task {
                        await model.reloadConfig()
                        await model.refreshCoreState()
                    }
                }
                .controlSize(.small)
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
        }
    }

    // MARK: - Runtime

    private var runtimeSection: some View {
        MenuSection("Runtime") {
            MenuRow("Core Details", systemImage: "info.circle",
                    value: model.core?.state.label ?? "—",
                    showsChevron: true) {
                model.path.append(.coreDetails)
            }
            MenuRow("Core Mode", systemImage: "gearshape",
                    value: coreModeValue,
                    showsChevron: true) {
                model.path.append(.coreMode)
            }
        }
    }

    private var coreModeValue: String {
        if model.coreModeLabel == "Daemon" {
            return model.daemon?.summary == "Active" ? "Daemon" : "Daemon"
        }
        return "Classic"
    }

    // MARK: - Network

    private var networkSection: some View {
        MenuSection("Network") {
            MenuRow("Proxies", systemImage: "arrow.triangle.branch",
                    value: proxySummary,
                    showsChevron: true) {
                model.path.append(.proxies)
            }
            .disabled(model.core?.state != .running)
            .help(model.core?.state == .running
                  ? "Choose a proxy group and node."
                  : "Start the core to choose a proxy.")
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
            return "Choose…"
        }
        if let delay = summary.delayLabel {
            return "\(summary.label) · \(delay)"
        }
        return summary.label
    }

    // MARK: - Navigation

    private var navigationSection: some View {
        MenuSection("Manage") {
            MenuRow("Subscriptions", systemImage: "arrow.down.circle",
                    value: subscriptionSummary,
                    showsChevron: true) {
                model.path.append(.subscriptions)
            }
            MenuRow("More", systemImage: "ellipsis.circle", showsChevron: true) {
                model.path.append(.more)
            }
        }
    }

    private var subscriptionSummary: String {
        let count = model.subscriptions.count
        if count == 0 { return "None" }
        return count == 1 ? "1" : "\(count)"
    }
}

// MARK: - Shared pieces

/// The status dot and label.
struct StatusLine: View {
    let state: CoreState?
    var error: String?

    var body: some View {
        HStack(spacing: 6) {
            Circle()
                .fill(color)
                .frame(width: 8, height: 8)
            Text(label)
                .font(.callout.weight(.medium))
                .foregroundStyle(.primary)
        }
        .help(error ?? "")
    }

    private var label: String {
        guard let state else { return "Connecting…" }
        return state.label
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
                .font(.system(size: 12))
            Text(message)
                .font(.caption)
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
