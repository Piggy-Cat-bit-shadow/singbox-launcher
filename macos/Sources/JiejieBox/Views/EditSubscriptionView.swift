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
                if let fetched = sub.nodes_fetched, fetched > 0, fetched != sub.node_count {
                    DetailLine(label: "Fetched", value: "\(fetched)")
                }
                if let code = sub.http_status_code, code != 0 {
                    DetailLine(label: "HTTP", value: "\(code)")
                }
                if let success = sub.last_success, !success.isEmpty {
                    DetailLine(label: "Last success", value: RelativeTime.describe(success))
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
                LabeledField(label: "URL",
                             placeholder: sub.url,
                             state: urlField)
            }

            MenuSection("Actions") {
                MenuRow("Save Changes",
                        subtitle: hasEdits ? nil : "Nothing to save yet.",
                        systemImage: "checkmark") {
                    Task {
                        let ok = await model.updateSubscription(
                            id: sub.id,
                            name: nameField.text.trimmingCharacters(in: .whitespaces),
                            url: urlField.text.trimmingCharacters(in: .whitespaces))
                        if ok { model.goBack() }
                    }
                }
                .disabled(model.pending != nil || !hasEdits)

                MenuRow(sub.enabled ? "Disable" : "Enable",
                        systemImage: sub.enabled ? "pause.circle" : "play.circle") {
                    Task { await model.setSubscriptionEnabled(sub.id, enabled: !sub.enabled) }
                }
                .disabled(model.pending != nil)

                MenuRow("Refresh Now", systemImage: "arrow.clockwise") {
                    Task { await model.refreshSubscription(sub.id) }
                }
                .disabled(model.pending != nil)

                if model.pending == .refreshingSubscription {
                    PendingRow("Fetching…")
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
        if urlField.text.isEmpty { urlField.text = sub.url }
    }

    /// True when a field differs from the stored record.
    private var hasEdits: Bool {
        guard let sub else { return false }
        let name = nameField.text.trimmingCharacters(in: .whitespaces)
        let url = urlField.text.trimmingCharacters(in: .whitespaces)
        return (name != sub.name && !name.isEmpty) || (url != sub.url && !url.isEmpty)
    }
}

/// A read-only label/value line.
///
/// Not a MenuRow: it is not interactive, and rendering it as a button would
/// create exactly the "looks clickable but does nothing" defect the row
/// primitives exist to avoid.
struct DetailLine: View {
    enum Tone { case normal, error }

    let label: String
    let value: String
    var tone: Tone = .normal

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer(minLength: 8)
            Text(value)
                .foregroundStyle(tone == .error ? Color.red : Color.primary)
                .multilineTextAlignment(.trailing)
                .lineLimit(3)
        }
        .font(.callout)
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 5)
    }
}
