// AddSubscriptionView — enter a URL, optionally name it.
//
// Two fields only. The URL is required; the name is not, because forcing a
// display name on someone who just wants to paste a link is friction, and the
// provider's own profile title replaces it after the first fetch anyway.

import SwiftUI

struct AddSubscriptionView: View {
    let model: AppModel

    // Plain stored properties rather than @State: this toolchain cannot
    // compile the SwiftUI macro plugin (see AppModel's header comment).
    private let urlField = FieldState()
    private let nameField = FieldState()

    var body: some View {
        PanelScaffold(model: model, title: "Add Subscription", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                MenuSection("Address") {
                    LabeledField(label: "URL",
                                 placeholder: "https://example.com/subscribe",
                                 state: urlField)
                    LabeledField(label: "Name",
                                 placeholder: "Optional — defaults to the host",
                                 state: nameField)
                }

                VStack(alignment: .leading, spacing: 6) {
                    Button {
                        add()
                    } label: {
                        if model.pending == .addingSubscription {
                            HStack(spacing: 6) {
                                ProgressView().controlSize(.small)
                                Text("Adding…")
                            }
                            .frame(maxWidth: .infinity)
                        } else {
                            Text("Add")
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .controlSize(.large)
                    .buttonStyle(.borderedProminent)
                    .disabled(!canAdd)

                    Text("The subscription is saved even if the first fetch fails; "
                         + "you can refresh it later.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                .padding(.horizontal, Metrics.rowPaddingH)
            }
            .padding(.vertical, 10)
        }
    }

    /// Valid only with a plausible URL — the same rule the backend enforces, so
    /// the button and the error cannot disagree.
    private var canAdd: Bool {
        guard model.pending == nil else { return false }
        let url = urlField.text.trimmingCharacters(in: .whitespaces)
        return url.hasPrefix("http://") || url.hasPrefix("https://")
    }

    private func add() {
        let url = urlField.text.trimmingCharacters(in: .whitespaces)
        let name = nameField.text.trimmingCharacters(in: .whitespaces)
        Task {
            // Pop only on success: leaving the form open on failure keeps the
            // user's typed URL available to correct.
            if await model.addSubscription(name: name, url: url) {
                model.goBack()
            }
        }
    }
}

/// Editable text state for a field.
///
/// A reference type so the field can be a plain property on the view; the
/// Observation macro is available even though the SwiftUI one is not.
@Observable
final class FieldState {
    var text: String = ""
}

/// A labelled text field sized for the panel.
struct LabeledField: View {
    let label: String
    var placeholder: String = ""
    let state: FieldState
    /// Rendered as a secure field when true (used for the pairing invite when
    /// the user prefers not to see it on screen).
    var secure: Bool = false

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(label)
                .font(.caption)
                .foregroundStyle(.secondary)
            Group {
                if secure {
                    SecureField(placeholder, text: Binding(get: { state.text },
                                                            set: { state.text = $0 }))
                } else {
                    TextField(placeholder, text: Binding(get: { state.text },
                                                         set: { state.text = $0 }))
                }
            }
            .textFieldStyle(.roundedBorder)
            .font(.callout)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 4)
    }
}
