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
    @Environment(\.localization) private var language

    var body: some View {
        PanelScaffold(model: model, title: L.subscriptions.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                if model.shouldShowBackendDown {
                    // The list is unknown, not empty: do not report "No
                    // Subscriptions" for a backend that never answered.
                    BackendDownView(model: model)
                } else if model.subscriptions.isEmpty && !model.subscriptionsLoading {
                    emptyState
                } else {
                    MenuSection {
                        addMenu
                    }

                    MenuSection(L.sources.tr(language)) {
                        ForEach(model.subscriptions) { sub in
                            subscriptionRow(sub)
                        }
                        if model.subscriptionsLoading {
                            PendingRow(L.loading.tr(language))
                        }
                    }

                    MenuSection {
                        MenuRow(L.updateAll.tr(language), systemImage: "arrow.down.circle") {
                            Task { await model.updateAllSubscriptions() }
                        }
                        .disabled(model.subscriptions.isEmpty || model.pending != nil)
                        if model.pending == .updatingSubscriptions {
                            PendingRow(L.updatingSubscriptions.tr(language))
                        }
                        if model.core?.config_stale == true {
                            reloadPrompt
                        }
                    }
                }

                if let status = model.transientStatus, !status.isEmpty {
                    Text(status)
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                }
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
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
            MenuAction(id: "url", title: L.addFromURL.tr(language), systemImage: "link") {
                model.path.append(.addSubscription)
            },
        ]
        if model.localSubscriptionImportAvailable {
            actions.append(
                MenuAction(id: "file", title: L.importFromFile.tr(language),
                           systemImage: "doc.badge.plus") {
                    importFromFile()
                }
            )
        }
        return MenuActionRow(title: L.addSubscription.tr(language),
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
                value: sub.nodeSummary(language),
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
                    ? L.saving.tr(language)
                    : (sub.enabled ? L.enabled.tr(language) : L.disabled.tr(language)),
                value: nil,
                isPending: model.pending == .updatingSetting(.subscriptionEnabled(sub.id)),
                weight: 1,
                help: sub.enabled
                    ? L.enabledHelp.tr(language)
                    : L.disabledHelp.tr(language),
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
            if sub.hasError { return "\(L.coreError.tr(language)) · \(sub.statusSummary(language))" }
            return sub.sourceSummary(language)
        }
        if sub.hasError {
            return "\(L.coreError.tr(language)) · \(sub.statusSummary(language))"
        }
        return sub.statusSummary(language)
    }

    // MARK: - Empty state

    private var emptyState: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(L.noSubscriptions.tr(language))
                .font(Typography.rowTitle.weight(.medium))
            Text(model.localSubscriptionImportAvailable
                 ? L.noSubscriptionsHintLocal.tr(language)
                 : L.noSubscriptionsHint.tr(language))
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
            HStack(spacing: 8) {
                Button {
                    model.path.append(.addSubscription)
                } label: {
                    Label(L.addFromURL.tr(language), systemImage: "plus")
                }
                .controlSize(.regular)
                .disabled(model.pending != nil)

                if model.localSubscriptionImportAvailable {
                    Button {
                        importFromFile()
                    } label: {
                        Label(L.importFromFileButton.tr(language), systemImage: "doc.badge.plus")
                    }
                    .controlSize(.regular)
                    .disabled(model.pending != nil)
                }
            }
            if model.pending == .importingSubscription {
                PendingRow(L.importingFromFile.tr(language))
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
                 ? L.reloadPromptTitle.tr(language)
                 : L.externalPromptTitle.tr(language))
                .font(Typography.rowValue.weight(.medium))
            Text(model.configRebuildable
                 ? L.reloadPromptBody.tr(language)
                 : L.externalPromptBody.tr(language))
                .font(Typography.rowSubtitle)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            if model.configRebuildable {
                Button {
                    Task {
                        await model.reloadConfig()
                        await model.refreshCoreState()
                    }
                } label: {
                    Text(L.reloadConfigAction.tr(language))
                }
                .controlSize(.small)
                .disabled(model.pending != nil)
            } else {
                Button {
                    model.revealConfig()
                } label: {
                    Text(L.openConfig.tr(language))
                }
                .controlSize(.small)
            }
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 6)
    }
}
