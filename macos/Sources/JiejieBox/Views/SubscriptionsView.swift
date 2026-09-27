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
                        addMenu
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

                if let status = model.transientStatus, !status.isEmpty {
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

    /// Add a source, from a URL or from a file.
    ///
    /// Both paths produce the same kind of source, so they share one entry
    /// point instead of two rows competing for the same intent. The file option
    /// appears only when the backend reports it can do the import: a row that
    /// leads to a guaranteed refusal is worse than no row.
    private var addMenu: some View {
        var actions: [MenuAction] = [
            MenuAction(id: "url", title: "Add from URL…", systemImage: "link") {
                model.path.append(.addSubscription)
            },
        ]
        if model.localSubscriptionImportAvailable {
            actions.append(
                MenuAction(id: "file", title: "Import from File…",
                           systemImage: "doc.badge.plus") {
                    importFromFile()
                }
            )
        }
        return MenuActionRow(title: "Add Subscription",
                             systemImage: "plus",
                             actions: actions,
                             disabled: model.pending != nil)
    }

    /// Ask for a file and hand the path to the backend.
    ///
    /// A cancelled panel is not a failure and reports nothing; every rejected
    /// file is explained by the backend, which is the only side that parsed it.
    private func importFromFile() {
        guard let path = FilePicker.chooseSubscriptionFile() else { return }
        Task { await model.importSubscriptionFile(path: path) }
    }

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
                // Shows its own saving state, so the row that was clicked
                // reports progress instead of the whole list going inert.
                title: model.pending == .updatingSetting(.subscriptionEnabled(sub.id))
                    ? "Saving…"
                    : (sub.enabled ? "On" : "Off"),
                value: nil,
                isPending: model.pending == .updatingSetting(.subscriptionEnabled(sub.id)),
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
        // A local snapshot has no fetch status to report: its "updated" time is
        // when it was imported, and its origin is a file rather than a URL. The
        // source line names the file so the row does not show an empty URL,
        // which would read as a broken source.
        if sub.isLocalSnapshot {
            if sub.hasError { return "Error · \(sub.statusSummary)" }
            return sub.sourceSummary
        }
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
            Text(model.localSubscriptionImportAvailable
                 ? "Add a subscription URL, or import a file from disk."
                 : "Add a subscription URL to import proxy nodes.")
                .font(.caption)
                .foregroundStyle(.secondary)
            HStack(spacing: 8) {
                Button {
                    model.path.append(.addSubscription)
                } label: {
                    Label("Add from URL", systemImage: "plus")
                }
                .controlSize(.regular)
                .disabled(model.pending != nil)

                if model.localSubscriptionImportAvailable {
                    Button {
                        importFromFile()
                    } label: {
                        Label("Import from File", systemImage: "doc.badge.plus")
                    }
                    .controlSize(.regular)
                    .disabled(model.pending != nil)
                }
            }
            if model.pending == .importingSubscription {
                PendingRow("Importing from file…")
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 12)
    }

    /// Shown when the built config has fallen behind the edited sources.
    ///
    /// The product never rebuilds on its own, so the screen states the fact and
    /// offers the action instead of leaving the user to wonder why a new
    /// subscription produced no nodes.
    /// Prompts for the step that applies an edited subscription list.
    ///
    /// The action depends on ownership: for an externally managed config a
    /// rebuild is impossible, so pointing at Reload would offer a step that can
    /// only fail. That user gets the file instead.
    private var reloadPrompt: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(model.configRebuildable
                 ? "Configuration needs reload"
                 : "Configuration is managed externally")
                .font(.caption.weight(.medium))
            Text(model.configRebuildable
                 ? "Reload the config to apply the new node list."
                 : "These changes will not take effect until the external configuration is updated.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            if model.configRebuildable {
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
            } else {
                Button {
                    model.revealConfig()
                } label: {
                    Text("Open Config")
                }
                .controlSize(.small)
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 6)
    }
}
