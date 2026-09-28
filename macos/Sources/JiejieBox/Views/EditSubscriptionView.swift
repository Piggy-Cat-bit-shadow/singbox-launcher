// EditSubscriptionView — edit one source, see its health, refresh or delete it.
//
// Details and actions are separated: the top answers "what is this and is it
// working?", the middle lets the user change name/URL/enabled, and the bottom
// holds the actions, with deletion behind a confirmation.

import SwiftUI

struct EditSubscriptionView: View {
    let model: AppModel
    let id: String
    @Environment(\.localization) private var language

    // Drafts live on the MODEL, keyed by subscription id: a stored property on a
    // View struct is replaced on every parent body pass, and keying by id keeps
    // one source's uncommitted edit from appearing in another's form.
    private var nameField: TextDraft { model.drafts.draft(DraftStore.editName(id)) }
    private var urlField: TextDraft { model.drafts.draft(DraftStore.editURL(id)) }
    private var confirmDelete: TextDraft { model.drafts.draft(DraftStore.editConfirmDelete(id)) }

    /// The live record, so the screen reflects the backend after a refresh
    /// rather than the values it was opened with.
    private var sub: Subscription? {
        model.subscriptions.first { $0.id == id }
    }

    var body: some View {
        PanelScaffold(model: model, title: L.subscriptions.tr(language),
                      onBack: { model.goBack() }) {
            if let sub {
                content(sub)
            } else {
                Text(L.noLongerConfigured.tr(language))
                    .font(Typography.rowValue)
                    .foregroundStyle(.secondary)
                    .padding(Metrics.rowPaddingH)
            }
        }
        .task {
            if model.subscriptions.isEmpty { await model.loadSubscriptions() }
            primeFields()
        }
        // The record can arrive after the first render (the list may still be
        // loading when this screen opens), and a form seeded from nothing would
        // show empty fields whose Save appears to do nothing. Re-seed once the
        // record exists, and only while the user has not typed into them.
        .onChange(of: model.subscriptions.count) { _, _ in primeFields() }
    }

    // MARK: - Content

    @ViewBuilder
    private func content(_ sub: Subscription) -> some View {
        VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
            MenuSection(L.status.tr(language)) {
                DetailLine(label: L.status.tr(language), value: sub.statusSummary(language),
                           tone: sub.hasError ? .error : .normal)
                DetailLine(label: L.nodes.tr(language), value: sub.nodeSummary(language))
                // A local snapshot came from a file, not a provider, so it has
                // no URL and no HTTP result to report. Showing "Source" as the
                // file and omitting the fetch-only rows keeps the screen honest
                // instead of printing blank fields that look like faults.
                if sub.isLocalSnapshot {
                    if let f = sub.filename, !f.isEmpty {
                        DetailLine(label: L.source.tr(language), value: f)
                    }
                }
                // The fetch-only rows below mean nothing for an imported file:
                // it was never fetched. The Status row above already carries
                // the import time for that case, so a second line would repeat
                // it under a misleading label.
                if !sub.isLocalSnapshot {
                    if let fetched = sub.nodes_fetched, fetched > 0, fetched != sub.node_count {
                        DetailLine(label: L.fetched.tr(language), value: "\(fetched)")
                    }
                    if let code = sub.http_status_code, code != 0 {
                        DetailLine(label: "HTTP", value: "\(code)")
                    }
                    if let success = sub.last_success, !success.isEmpty {
                        DetailLine(label: L.lastSuccess.tr(language), value: RelativeTime.describe(success))
                    }
                }
                if let support = sub.support_url, !support.isEmpty,
                   let url = URL(string: support) {
                    MenuRow(L.providerSupport.tr(language), systemImage: "questionmark.circle") {
                        NSWorkspace.shared.open(url)
                    }
                }
            }

            MenuSection(L.saveChanges.tr(language)) {
                LabeledField(label: L.name.tr(language),
                             placeholder: sub.name,
                             state: nameField)
                // No URL field for an imported file: it has no provider, and an
                // editable empty URL would either save nothing or imply the
                // source can be pointed somewhere. Renaming is what remains.
                if !sub.isLocalSnapshot {
                    LabeledField(label: L.url.tr(language),
                                 placeholder: sub.url,
                                 state: urlField)
                }
            }

            MenuSection(L.refresh.tr(language)) {
                MenuRow(L.saveChanges.tr(language),
                        subtitle: hasEdits ? nil : L.nothingToSave.tr(language),
                        systemImage: "checkmark") {
                    Task {
                        let ok = await model.updateSubscription(
                            id: sub.id,
                            // An emptied field means "clear the custom name",
                            // which is a real edit the user can intend — sent as
                            // an explicit flag, because an empty name on its own
                            // means "leave the name alone".
                            name: nameField.trimmed,
                            // A local snapshot keeps its stored URL (empty) and
                            // is not re-pointed by this form; sending the field
                            // would let a stale value overwrite it.
                            url: sub.isLocalSnapshot ? sub.url : urlField.trimmed,
                            // An emptied field means "clear the custom name",
                            // which is a real edit the user can intend — sent as
                            // an explicit flag, because an empty name on its own
                            // means "leave the name alone".
                            clearName: nameField.trimmed.isEmpty && sub.hasCustomName)
                        if ok {
                            model.clearEditDrafts(id: sub.id)
                            model.popIfCurrent(.editSubscription(id))
                        }
                    }
                }
                .disabled(model.pending != nil || !hasEdits)

                MenuRow(sub.enabled ? L.disable.tr(language) : L.enable.tr(language),
                        systemImage: sub.enabled ? "pause.circle" : "play.circle") {
                    Task { await model.setSubscriptionEnabled(sub.id, enabled: !sub.enabled) }
                }
                .disabled(model.pending != nil)

                // Refresh is offered only where it can succeed. A local
                // snapshot has no provider to fetch from; the backend refuses
                // it, so the row is omitted and the reason is stated instead.
                if sub.isRefreshable {
                    MenuRow(L.refreshNow.tr(language), systemImage: "arrow.clockwise") {
                        Task { await model.refreshSubscription(sub.id) }
                    }
                    .disabled(model.pending != nil)

                    if model.pending == .refreshingSubscription {
                        PendingRow(L.fetching.tr(language))
                    }
                } else {
                    // Stated as a fact rather than a dead button: the source has
                    // no provider, so there is nothing to fetch. A greyed-out
                    // "Refresh" would imply the action exists but is temporarily
                    // unavailable, which is not the case.
                    DetailLine(label: L.refreshNotAvailable.tr(language),
                               value: L.refreshNotAvailableWhy.tr(language))
                }
            }

            MenuSection(L.delete.tr(language)) {
                MenuRow(L.deleteSubscription.tr(language), systemImage: "trash", role: .destructive) {
                    confirmDelete.text = "confirm"
                }
                .disabled(model.pending != nil)
            }
        }
        .padding(.vertical, 8)
        .confirmationDialog(L.deleteSubscriptionConfirm.tr(language),
                            isPresented: Binding(
                                get: { confirmDelete.text == "confirm" },
                                set: { if !$0 { confirmDelete.text = "" } }),
                            titleVisibility: .visible) {
            Button(L.delete.tr(language), role: .destructive) {
                confirmDelete.text = ""
                Task {
                    if await model.removeSubscription(sub.id) {
                        model.clearEditDrafts(id: sub.id)
                        model.popIfCurrent(.editSubscription(sub.id))
                    }
                }
            }
            Button(L.cancel.tr(language), role: .cancel) { confirmDelete.text = "" }
        } message: {
            Text(L.deleteSubscriptionMessage.tr(language))
        }
    }

    /// Seed the editable fields from the current record, once.
    ///
    /// Only fills a field the user has not typed into, so a late-arriving
    /// reload cannot overwrite an edit in progress.
    private func primeFields() {
        guard let sub else { return }
        // Seeded only once per draft: `isEmpty` is the guard, so a user who has
        // deliberately cleared the name is not re-seeded with the stored one on
        // the next reload — which would undo the edit they are in the middle of.
        if !model.drafts.hasContent(DraftStore.editName(id)) {
            model.drafts.set(DraftStore.editName(id), sub.name)
        }
        // The URL field is not shown for a local snapshot, so seeding it would
        // leave a value that only exists to be compared against.
        if !sub.isLocalSnapshot, !model.drafts.hasContent(DraftStore.editURL(id)) {
            model.drafts.set(DraftStore.editURL(id), sub.url)
        }
    }

    /// True when a field differs from the stored record.
    ///
    /// An EMPTIED name counts as an edit, because clearing a custom name is a real
    /// intention: it hands the source back to its provider/default title. The
    /// previous version required `!name.isEmpty` and so could never express it —
    /// with a custom name set, deleting the text left Save disabled and the user
    /// had no way back to the default. The backend now takes an emptied name as
    /// an explicit clear (see `UpdateSubscription`).
    ///
    /// An emptied URL is still not an edit: a subscription must have a URL, so
    /// there is nothing to save and the backend would reject it.
    private var hasEdits: Bool {
        guard let sub else { return false }
        if nameField.trimmed != sub.name { return true }
        guard !sub.isLocalSnapshot else { return false }
        let url = urlField.trimmed
        return url != sub.url && !url.isEmpty
    }
}
