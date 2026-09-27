// BackendDownView — what every data screen shows when the backend is not
// answering.
//
// Why this is shared rather than per-screen: without it, a crashed backend
// renders as a business empty state. "No Subscriptions" and "No nodes" are
// then indistinguishable from "the app cannot reach its backend", which sends
// the user looking for a problem in their configuration that does not exist.
//
// The rule is: a screen never reports emptiness until it has confirmed the
// backend is alive. Absence of data is only meaningful once someone answered.

import SwiftUI

/// Shown in place of content when the backend is unreachable.
struct BackendDownView: View {
    let model: AppModel
    /// What this screen would have shown, so the message stays specific.
    var subject: String = "this screen"

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                Image(systemName: "bolt.horizontal.circle")
                    .foregroundStyle(.secondary)
                Text("Backend Unavailable")
                    .font(.callout.weight(.medium))
            }

            Text(reason)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            Button {
                Task { await model.restart() }
            } label: {
                if model.connection == .connecting {
                    HStack(spacing: 6) {
                        ProgressView().controlSize(.small)
                        Text("Restarting…")
                    }
                } else {
                    Text("Restart Backend")
                }
            }
            .controlSize(.small)
            .disabled(model.connection == .connecting)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 12)
    }

    private var reason: String {
        switch model.connection {
        case .failed(let message):
            return message
        case .connecting:
            return "Reconnecting. \(subject) will reload automatically."
        case .idle:
            return "The backend is not running, so \(subject) cannot be loaded."
        case .ready:
            // Reachable but a specific call failed; the screen's own error
            // banner covers that case.
            return "The backend did not answer. Try again in a moment."
        }
    }
}

extension AppModel {
    /// True when a screen should show `BackendDownView` INSTEAD of its own
    /// empty state.
    ///
    /// Kept on the model so every screen applies the same rule, rather than
    /// each view inventing its own guard and drifting.
    var shouldShowBackendDown: Bool {
        connection != .ready
    }
}
