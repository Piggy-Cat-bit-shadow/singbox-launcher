// SubscriptionsView — manage the sources that supply proxy nodes.
//
// This is the screen the old design was missing: "Update Subscriptions" existed
// with nowhere to enter a subscription. It is a manager, not a configurator —
// name, URL, enabled, refresh, delete — because the full configurator is not
// part of the menu bar product.
//
// Update All lives here rather than in More, so the button that fetches
// everything sits on the screen that lists what "everything" is.

import SwiftUI

struct SubscriptionsView: View {
    let model: AppModel

    var body: some View {
        PanelScaffold(model: model, title: "Subscriptions", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                if model.shouldShowBackendDown {
                    // The list is unknown, not empty: do not report "No
                    // Subscriptions" for a backend that never answered.
                    BackendDownView(model: model, subject: "subscriptions")
                } else if model.subscriptions.isEmpty && !model.subscriptionsLoading {
                    emptyState
                } else {
                    MenuSection {
                        MenuRow("Add Subscription", systemImage: "plus") {
                            model.path.append(.addSubscription)
                        }
                    }

                    MenuSection("Sources") {
                        ForEach(model.subscriptions) { sub in
                            subscriptionRow(sub)
                        }
                        if model.subscriptionsLoading {
                            PendingRow("Loading…")
                        }
                    }

                    MenuSection {
                        MenuRow("Update All", systemImage: "arrow.down.circle") {
                            Task { await model.updateAllSubscriptions() }
                        }
                        .disabled(model.subscriptions.isEmpty || model.pending != nil)
                        if model.pending == .updatingSubscriptions {
                            PendingRow("Updating subscriptions…")
                        }
                        if model.core?.config_stale == true {
                            reloadPrompt
                        }
                    }
                }

                if let status = model.transientStatus {
                    Text(status)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }
            }
            .padding(.vertical, 8)
        }
        .task { await model.loadSubscriptions() }
    }

    // MARK: - Rows

    /// One source: open it, or flip it on and off.
    ///
    /// Two real actions, so they are siblings rather than a switch drawn inside
    /// a navigating row. The previous version put a hit-disabled switch inside
    /// the row's button, so a switch that LOOKED tappable actually navigated —
    /// the "looks clickable but does something else" defect the row primitives
    /// exist to prevent.
    private func subscriptionRow(_ sub: Subscription) -> some View {
        ActionRow(actions: [
            RowAction(
                id: "open-\(sub.id)",
                title: sub.label,
                subtitle: subtitle(for: sub),
                value: sub.nodeSummary,
                showsChevron: true,
                weight: 4,
                help: "Edit this subscription.",
                action: { model.path.append(.editSubscription(sub.id)) }
            ),
            RowAction(
                id: "enabled-\(sub.id)",
                title: sub.enabled ? "On" : "Off",
                value: nil,
                weight: 1,
                help: sub.enabled
                    ? "Enabled. Click to exclude it from the built config."
                    : "Disabled. Click to include it again.",
                action: {
                    Task { await model.setSubscriptionEnabled(sub.id, enabled: !sub.enabled) }
                }
            ),
        ], disabled: model.pending != nil)
        .opacity(sub.enabled ? 1 : 0.6)
    }

    private func subtitle(for sub: Subscription) -> String {
        if sub.hasError {
            return "Error · \(sub.statusSummary)"
        }
        return sub.statusSummary
    }

    // MARK: - Empty state

    private var emptyState: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("No Subscriptions")
                .font(.callout.weight(.medium))
            Text("Add a subscription URL to import proxy nodes.")
                .font(.caption)
                .foregroundStyle(.secondary)
            Button {
                model.path.append(.addSubscription)
            } label: {
                Label("Add Subscription", systemImage: "plus")
            }
            .controlSize(.regular)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 12)
    }

    /// Shown when the built config has fallen behind the edited sources.
    ///
    /// The product never rebuilds on its own, so the screen states the fact and
    /// offers the action instead of leaving the user to wonder why a new
    /// subscription produced no nodes.
    private var reloadPrompt: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Configuration needs reload")
                .font(.caption.weight(.medium))
            Text("Reload the config to apply the new node list.")
                .font(.caption)
                .foregroundStyle(.secondary)
            Button {
                Task {
                    await model.reloadConfig()
                    await model.refreshCoreState()
                }
            } label: {
                Text("Reload Config")
            }
            .controlSize(.small)
            .disabled(model.pending != nil)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 6)
    }
}
