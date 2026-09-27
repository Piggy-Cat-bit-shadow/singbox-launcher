// EditSubscriptionView — edit one source, see its health, refresh or delete it.
//
// Details and actions are separated: the top answers "what is this and is it
// working?", the middle lets the user change name/URL/enabled, and the bottom
// holds the actions, with deletion behind a confirmation.

import SwiftUI

struct EditSubscriptionView: View {
    let model: AppModel
    let id: String

    private let nameField = FieldState()
    private let urlField = FieldState()
    private let confirmDelete = FieldState()

    /// The live record, so the screen reflects the backend after a refresh
    /// rather than the values it was opened with.
    private var sub: Subscription? {
        model.subscriptions.first { $0.id == id }
    }

    var body: some View {
        PanelScaffold(model: model, title: "Subscription", onBack: { model.goBack() }) {
            if let sub {
                content(sub)
            } else {
                Text("This subscription is no longer configured.")
                    .font(.callout)
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
        VStack(alignment: .leading, spacing: 10) {
            MenuSection("Details") {
                DetailLine(label: "Status", value: sub.statusSummary,
                           tone: sub.hasError ? .error : .normal)
                DetailLine(label: "Nodes", value: sub.nodeSummary)
                // A local snapshot came from a file, not a provider, so it has
                // no URL and no HTTP result to report. Showing "Source" as the
                // file and omitting the fetch-only rows keeps the screen honest
                // instead of printing blank fields that look like faults.
                if sub.isLocalSnapshot {
                    if let f = sub.filename, !f.isEmpty {
                        DetailLine(label: "Source", value: f)
                    }
                }
                // The fetch-only rows below mean nothing for an imported file:
                // it was never fetched. The Status row above already carries
                // the import time for that case, so a second line would repeat
                // it under a misleading label.
                if !sub.isLocalSnapshot {
                    if let fetched = sub.nodes_fetched, fetched > 0, fetched != sub.node_count {
                        DetailLine(label: "Fetched", value: "\(fetched)")
                    }
                    if let code = sub.http_status_code, code != 0 {
                        DetailLine(label: "HTTP", value: "\(code)")
                    }
                    if let success = sub.last_success, !success.isEmpty {
                        DetailLine(label: "Last success", value: RelativeTime.describe(success))
                    }
                }
                if let support = sub.support_url, !support.isEmpty,
                   let url = URL(string: support) {
                    MenuRow("Provider Support", systemImage: "questionmark.circle") {
                        NSWorkspace.shared.open(url)
                    }
                }
            }

            MenuSection("Edit") {
                LabeledField(label: "Name",
                             placeholder: sub.name,
                             state: nameField)
                // No URL field for an imported file: it has no provider, and an
                // editable empty URL would either save nothing or imply the
                // source can be pointed somewhere. Renaming is what remains.
                if !sub.isLocalSnapshot {
                    LabeledField(label: "URL",
                                 placeholder: sub.url,
                                 state: urlField)
                }
            }

            MenuSection("Actions") {
                MenuRow("Save Changes",
                        subtitle: hasEdits ? nil : "Nothing to save yet.",
                        systemImage: "checkmark") {
                    Task {
                        let ok = await model.updateSubscription(
                            id: sub.id,
                            name: nameField.text.trimmingCharacters(in: .whitespaces),
                            // A local snapshot keeps its stored URL (empty) and
                            // is not re-pointed by this form; sending the field
                            // would let a stale value overwrite it.
                            url: sub.isLocalSnapshot
                                ? sub.url
                                : urlField.text.trimmingCharacters(in: .whitespaces))
                        if ok { model.goBack() }
                    }
                }
                .disabled(model.pending != nil || !hasEdits)

                MenuRow(sub.enabled ? "Disable" : "Enable",
                        systemImage: sub.enabled ? "pause.circle" : "play.circle") {
                    Task { await model.setSubscriptionEnabled(sub.id, enabled: !sub.enabled) }
                }
                .disabled(model.pending != nil)

                // Refresh is offered only where it can succeed. A local
                // snapshot has no provider to fetch from; the backend refuses
                // it, so the row is omitted and the reason is stated instead.
                if sub.isRefreshable {
                    MenuRow("Refresh Now", systemImage: "arrow.clockwise") {
                        Task { await model.refreshSubscription(sub.id) }
                    }
                    .disabled(model.pending != nil)

                    if model.pending == .refreshingSubscription {
                        PendingRow("Fetching…")
                    }
                } else {
                    // Stated as a fact rather than a dead button: the source has
                    // no provider, so there is nothing to fetch. A greyed-out
                    // "Refresh" would imply the action exists but is temporarily
                    // unavailable, which is not the case.
                    DetailLine(label: "Refresh",
                               value: "Not available for an imported file")
                }
            }

            MenuSection("Danger") {
                MenuRow("Delete Subscription", systemImage: "trash", role: .destructive) {
                    confirmDelete.text = "confirm"
                }
                .disabled(model.pending != nil)
            }
        }
        .padding(.vertical, 8)
        .confirmationDialog("Delete this subscription?",
                            isPresented: Binding(
                                get: { confirmDelete.text == "confirm" },
                                set: { if !$0 { confirmDelete.text = "" } }),
                            titleVisibility: .visible) {
            Button("Delete", role: .destructive) {
                confirmDelete.text = ""
                Task {
                    if await model.removeSubscription(sub.id) { model.goBack() }
                }
            }
            Button("Cancel", role: .cancel) { confirmDelete.text = "" }
        } message: {
            Text("Its nodes will be removed from the configuration on the next reload.")
        }
    }

    /// Seed the editable fields from the current record, once.
    ///
    /// Only fills a field the user has not typed into, so a late-arriving
    /// reload cannot overwrite an edit in progress.
    private func primeFields() {
        guard let sub else { return }
        if nameField.text.isEmpty { nameField.text = sub.name }
        // The URL field is not shown for a local snapshot, so seeding it would
        // leave a value that only exists to be compared against.
        if urlField.text.isEmpty && !sub.isLocalSnapshot { urlField.text = sub.url }
    }

    /// True when a field differs from the stored record.
    private var hasEdits: Bool {
        guard let sub else { return false }
        let name = nameField.text.trimmingCharacters(in: .whitespaces)
        if name != sub.name && !name.isEmpty { return true }
        guard !sub.isLocalSnapshot else { return false }
        let url = urlField.text.trimmingCharacters(in: .whitespaces)
        return url != sub.url && !url.isEmpty
    }
}
