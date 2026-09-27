// CoreModeView — switch between the classic and daemon core engines.
//
// The rules come from the backend, not from this view:
//
//   - Switching is refused while the core runs (a live classic process cannot
//     be handed to the daemon and vice versa). The row is disabled and says so
//     rather than failing after the click.
//   - Daemon is offered only when this build reports the capability.
//   - The result is reported by the backend; the checkmark follows
//     settings_changed, never an optimistic local flip.

import SwiftUI

struct CoreModeView: View {
    let model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if coreIsRunning {
                Banner(kind: .warning,
                       message: "Stop the core before changing the mode.") {
                    EmptyView()
                }
            }

            MenuSection("Engine") {
                ForEach(modes, id: \.id) { mode in
                    MenuRow(
                        mode.title,
                        subtitle: mode.detail,
                        systemImage: mode.id == activeID ? "checkmark.circle.fill" : "circle",
                        action: { Task { await model.setCoreMode(mode.id) } },
                        trailing: {
                            if isSwitching(to: mode.id) {
                                ProgressView().controlSize(.small)
                            }
                        }
                    )
                    .disabled(!canSwitch(to: mode.id))
                    .help(helpText(for: mode.id))
                }
            }

            Text("The mode is saved with your settings and applies the next time the core starts.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(.horizontal, Metrics.rowPaddingH)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.vertical, 8)
    }

    private var activeID: String {
        model.settings?.core_backend_mode ?? "classic"
    }

    private var coreIsRunning: Bool {
        model.core?.state == .running
    }

    /// Classic is always offered; daemon only when the build supports it.
    private var modes: [(id: String, title: String, detail: String)] {
        var out: [(id: String, title: String, detail: String)] = [
            ("classic", "Classic", "The launcher runs sing-box directly."),
        ]
        if model.daemonAvailable {
            out.append(("daemon", "Daemon",
                        "A background service runs the core and keeps it alive."))
        }
        return out
    }

    private func isSwitching(to id: String) -> Bool {
        model.pending == .switchingMode(id)
    }

    private func canSwitch(to id: String) -> Bool {
        guard model.pending == nil else { return false }
        guard !coreIsRunning else { return false }
        return id != activeID
    }

    private func helpText(for id: String) -> String {
        if coreIsRunning { return "Stop the core before changing the mode." }
        if id == activeID { return "This mode is already active." }
        if model.pending != nil { return "An operation is already in progress." }
        return "Switch to \(id)."
    }
}
