// CoreModeView — choose the engine.
//
// Classic and Daemon are deliberately NOT presented as two equal radio options.
// They have different prerequisites: classic works immediately as a child
// process, while the daemon needs an installed, paired and reachable system
// service. Rendering them as symmetric choices is what made picking Daemon
// appear to hang — it tried to activate an engine that was never set up.
//
// So the Daemon row is a doorway: when the engine is ready it activates, and
// when it is not it opens the Daemon screen, which owns the setup sequence.

import SwiftUI

struct CoreModeView: View {
    let model: AppModel

    var body: some View {
        PanelScaffold(model: model, title: "Core Mode", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                MenuSection("Engine") {
                    classicRow
                    daemonRow
                }

                if let reason = model.coreModeBlockedReason {
                    Text(reason)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }

                if model.coreModePreferenceDiverged {
                    // The running engine and the saved preference disagree:
                    // the switch worked but the choice did not persist.
                    Text("Running \(model.coreModeLabel) (saved preference: \(model.savedCoreModeLabel)).")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }

                if model.pending == .switchingMode("classic")
                    || model.pending == .switchingMode("daemon") {
                    PendingRow("Switching engine…")
                }
            }
            .padding(.vertical, 8)
        }
        .task {
            // The daemon row's subtitle is a status claim, so it is loaded
            // rather than guessed.
            if model.daemon == nil { await model.loadDaemonStatus() }
        }
    }

    // MARK: - Rows

    private var classicRow: some View {
        MenuRow("Classic",
                subtitle: "sing-box runs as a child of JiejieBox.",
                action: {
                    guard model.coreModeLabel != "Classic" else { return }
                    Task { await model.activateClassicMode() }
                },
                trailing: { activeBadge(model.coreModeLabel == "Classic") })
            .disabled(!model.canSwitchCoreMode || model.coreModeLabel == "Classic")
    }

    /// The daemon row opens the Daemon screen unless the engine is ready and
    /// inactive, in which case it activates directly.
    private var daemonRow: some View {
        MenuRow("Daemon",
                subtitle: daemonSubtitle,
                showsChevron: !daemonActive && !daemonReady,
                action: {
                    if daemonActive || !daemonReady {
                        model.path.append(.daemon)
                    } else {
                        Task { await model.activateDaemonMode() }
                    }
                },
                trailing: { activeBadge(daemonActive) })
            .disabled(!model.canSwitchCoreMode || daemonActive)
    }

    @ViewBuilder
    private func activeBadge(_ active: Bool) -> some View {
        if active {
            Text("Active")
                .font(.caption.weight(.medium))
                .foregroundStyle(.tint)
        }
    }

    // MARK: - Daemon state

    private var daemonActive: Bool { model.coreModeLabel == "Daemon" }

    private var daemonReady: Bool { model.daemon?.ready == true }

    /// Says what the engine's state actually is, so the row answers "can I use
    /// this?" before it is clicked.
    private var daemonSubtitle: String {
        guard let status = model.daemon else {
            return "sing-box runs as a persistent system service."
        }
        if !status.supported {
            return "Not available in this build."
        }
        if daemonActive {
            return "Running as a system service."
        }
        // A core without `lxd` cannot host the service at all, and saying
        // "setup required" would send the user to a screen that cannot help.
        if !status.core_supports_lxd {
            return "The installed core has no daemon support."
        }
        switch status.summary {
        case "Ready":
            return "Service ready. Click to switch."
        case "Setup required":
            return "Not installed. Click to set up."
        case "Service stopped":
            return "Installed but stopped. Click to set up."
        case "Pairing required":
            return "Not paired. Click to set up."
        default:
            return "Service unreachable. Click for details."
        }
    }
}
