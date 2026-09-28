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
    @Environment(\.localization) private var language

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                Image(systemName: "bolt.horizontal.circle")
                    .foregroundStyle(.secondary)
                Text(L.backendUnavailable.tr(language))
                    .font(Typography.rowTitle.weight(.medium))
            }

            Text(reason)
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            Button {
                Task { await model.restart() }
            } label: {
                if model.connection == .connecting {
                    HStack(spacing: 6) {
                        ProgressView().controlSize(.small)
                        Text(L.reconnecting.tr(language))
                    }
                } else {
                    Text(L.restartBackend.tr(language))
                }
            }
            .controlSize(.small)
            // A restart that is under way leaves `connection` at `.failed`
            // until the helper is actually gone, so the connection state alone
            // cannot describe it — the in-flight flag is what closes the window.
            .disabled(model.connection == .connecting || model.backendRestartInFlight)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 12)
    }

    /// Why the backend is unavailable.
    ///
    /// A backend-authored failure message is shown verbatim: it comes from the Go
    /// side already localized by that side's own locale package, and the frontend
    /// has neither the context nor the vocabulary to re-translate it.
    private var reason: String {
        switch model.connection {
        case .failed(let message):
            return message
        case .connecting:
            return L.reconnecting.tr(language)
        case .idle:
            return L.backendUnavailableHint.tr(language)
        case .ready:
            // Reachable but a specific call failed; the screen's own error
            // banner covers that case.
            return L.backendUnavailableHint.tr(language)
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
