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
                if model.subscriptions.isEmpty && !model.subscriptionsLoading {
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

    private func subscriptionRow(_ sub: Subscription) -> some View {
        MenuRow(sub.label,
                subtitle: subtitle(for: sub),
                value: sub.nodeSummary,
                showsChevron: true) {
            model.path.append(.editSubscription(sub.id))
        } trailing: {
            // The enable switch is part of the row, not a separate target: the
            // whole row navigates, and the switch reflects state.
            Toggle("", isOn: Binding(
                get: { sub.enabled },
                set: { value in Task { await model.setSubscriptionEnabled(sub.id, enabled: value) } }
            ))
            .labelsHidden()
            .toggleStyle(.switch)
            .controlSize(.small)
            .allowsHitTesting(false)
        }
        .opacity(sub.enabled ? 1 : 0.55)
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
