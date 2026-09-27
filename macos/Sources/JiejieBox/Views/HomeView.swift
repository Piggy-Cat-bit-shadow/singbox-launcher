// HomeView — the menu-bar panel.
//
// Information hierarchy, top to bottom:
//   identity + status → primary action → banners → runtime → navigation
//
// Every row is a MenuRow, so hit targets and feedback are consistent. The
// window is ~400pt wide: wide enough for real information, still clearly a
// menu-bar utility rather than a main window.

import SwiftUI

struct HomeView: View {
    let model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            header
            primaryAction
            banners
            runtimeSection
            navigationSection
            Divider().padding(.horizontal, Metrics.sectionPaddingH)
            footer
        }
        .padding(.vertical, 10)
    }

    // MARK: - Identity and status

    private var header: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text("JiejieBox")
                    .font(.headline)
                Spacer(minLength: 0)
                // The version is the one string here that can be arbitrarily
                // long (a custom core can carry any tag), so it is the one
                // that truncates and yields space to the live speed.
                if let version = model.core?.core_version, !version.isEmpty {
                    Text(version)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .layoutPriority(-1)
                        .help(version)
                }
            }
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                StatusLine(state: model.core?.state, error: model.core?.error_message)
                Spacer(minLength: 0)
                if model.core?.state == .running, let rate = model.traffic {
                    speedReadout(rate)
                }
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    /// Live up/down speed, shown only while connected.
    ///
    /// Monospaced digits so the numbers do not jitter the layout as they
    /// change every second.
    private func speedReadout(_ rate: TrafficRate) -> some View {
        HStack(spacing: 6) {
            Label(ByteFormat.rate(rate.down), systemImage: "arrow.down")
                .foregroundStyle(.secondary)
            Label(ByteFormat.rate(rate.up), systemImage: "arrow.up")
                .foregroundStyle(.secondary)
        }
        .font(.caption.monospacedDigit())
        .labelStyle(.titleAndIcon)
        .lineLimit(1)
        .help("Total since connect: ↓ \(ByteFormat.size(rate.total_down)) · ↑ \(ByteFormat.size(rate.total_up))")
    }

    // MARK: - Primary action

    private var primaryAction: some View {
        Button {
            Task { await model.toggleCore() }
        } label: {
            HStack(spacing: 6) {
                if let state = model.core?.state, state.isTransitioning {
                    ProgressView().controlSize(.small)
                }
                Text(primaryTitle)
                    .frame(maxWidth: .infinity)
            }
            .frame(minHeight: 34)
        }
        .controlSize(.large)
        .buttonStyle(.borderedProminent)
        .disabled(!canAct)
        .keyboardShortcut(.defaultAction)
        .padding(.horizontal, Metrics.rowPaddingH)
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

    /// Disabled only during a real transition, or when there is no core binary
    /// to start. The reason is surfaced in the button's help text rather than
    /// leaving a dead control unexplained.
    private var canAct: Bool {
        guard case .ready = model.connection else { return false }
        guard let core = model.core else { return false }
        if core.state.isTransitioning { return false }
        if core.state != .running && !core.binary_exists { return false }
        return true
    }

    private var primaryHelp: String {
        guard let core = model.core else { return "" }
        if core.state.isTransitioning { return "Please wait for the current operation to finish." }
        if core.state != .running && !core.binary_exists {
            return "The sing-box core binary was not found."
        }
        return core.state == .running ? "Stop the core" : "Start the core"
    }

    /// The node in use, so Home answers "what am I connected through?"
    /// without a trip to the Proxy screen.
    private var proxySummary: String {
        guard model.core?.state == .running else { return "—" }
        if let node = model.proxies.first(where: { $0.selected }) {
            return node.label
        }
        if let group = model.groups.first(where: { $0.name == model.selectedGroup }),
           let selected = group.selected_display ?? group.selected, !selected.isEmpty {
            return selected
        }
        return "Choose…"
    }

    // MARK: - Banners

    @ViewBuilder
    private var banners: some View {
        if case .failed(let message) = model.connection {
            Banner(kind: .error, message: message) {
                Button("Restart") { Task { await model.restart() } }
                    .controlSize(.small)
            }
        } else if let error = model.lastError {
            Banner(kind: .error, message: error) {
                Button {
                    model.clearError()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                }
                .buttonStyle(.plain)
                .help("Dismiss")
            }
        } else if let status = model.transientStatus {
            Banner(kind: .success, message: status) {
                EmptyView()
            }
        } else if model.coreMissing || model.configMissing {
            Banner(kind: .warning, message: missingRecoveryText) {
                Button("Open Folder") { model.revealConfigFolder() }
                    .controlSize(.small)
            }
        }
    }

    private var missingRecoveryText: String {
        if model.coreMissing { return "The sing-box core binary was not found." }
        return "config.json was not found. Reload or create one, then try again."
    }

    // MARK: - Runtime

    private var runtimeSection: some View {
        MenuSection("Runtime") {
            MenuRow("Proxies", systemImage: "arrow.triangle.branch",
                    value: proxySummary,
                    showsChevron: true) {
                model.path.append(.proxies)
            }
            .disabled(model.core?.state != .running)
            .help(model.core?.state == .running
                  ? "Choose a proxy group and node."
                  : "Start the core to choose a proxy.")
            MenuRow("Core Details", systemImage: "info.circle",
                    value: model.core?.state.label ?? "—",
                    showsChevron: true) {
                model.path.append(.coreDetails)
            }
            MenuRow("Core Mode", systemImage: "gearshape",
                    value: model.coreModeLabel,
                    showsChevron: true) {
                model.path.append(.coreMode)
            }
        }
    }

    // MARK: - Navigation

    private var navigationSection: some View {
        MenuSection {
            MenuRow("More", systemImage: "ellipsis.circle", showsChevron: true) {
                model.path.append(.more)
            }
        }
    }

    // MARK: - Footer

    private var footer: some View {
        HStack(spacing: 8) {
            Button("Reveal Config") { model.revealConfig() }
                .buttonStyle(.plain)
                .font(.callout)
                .help(model.settings?.config_path ?? "Config path unknown")
                .disabled(model.settings?.config_path.isEmpty ?? true)

            Spacer(minLength: 0)

            Button("Quit") {
                Task {
                    // The backend decides the core's fate: in daemon mode with
                    // keep-running on, the core is meant to outlive the GUI.
                    // Stopping it here would silently break that policy.
                    await model.quit()
                    NSApplication.shared.terminate(nil)
                }
            }
            .buttonStyle(.plain)
            .font(.callout)
            .keyboardShortcut("q")
        }
        .padding(.horizontal, Metrics.rowPaddingH)
    }
}

// MARK: - Shared pieces

/// Status dot plus label.
struct StatusLine: View {
    let state: CoreState?
    let error: String?

    var body: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(color)
                .frame(width: 7, height: 7)
            Text(state?.label ?? "Connecting…")
                .font(.subheadline)
                .foregroundStyle(.secondary)
        }
        .help(error ?? "")
    }

    private var color: Color {
        switch state {
        case .running: return .green
        case .starting, .stopping: return .orange
        case .error: return .red
        default: return .secondary
        }
    }
}

/// Inline message with an optional trailing action.
struct Banner<Action: View>: View {
    enum Kind { case warning, error, success }

    let kind: Kind
    let message: String
    @ViewBuilder var action: () -> Action

    var body: some View {
        HStack(alignment: .top, spacing: 6) {
            Image(systemName: symbol)
                .foregroundStyle(tint)
            Text(message)
                .font(.caption)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
            action()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 5)
        .background(
            RoundedRectangle(cornerRadius: Metrics.rowCorner)
                .fill(tint.opacity(0.08))
                .padding(.horizontal, Metrics.sectionPaddingH)
        )
    }

    private var symbol: String {
        switch kind {
        case .warning: return "exclamationmark.triangle.fill"
        case .error: return "exclamationmark.circle.fill"
        case .success: return "checkmark.circle.fill"
        }
    }

    private var tint: Color {
        switch kind {
        case .warning: return .orange
        case .error: return .red
        case .success: return .green
        }
    }
}
