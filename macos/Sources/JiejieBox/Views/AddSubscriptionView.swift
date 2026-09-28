// AddSubscriptionView — enter a URL, optionally name it.
//
// Two fields only. The URL is required; the name is not, because forcing a
// display name on someone who just wants to paste a link is friction, and the
// provider's own profile title replaces it after the first fetch anyway.

import SwiftUI

struct AddSubscriptionView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    // Plain stored properties rather than @State: this toolchain cannot
    // compile the SwiftUI macro plugin (see AppModel's header comment).
    private let urlField = FieldState()
    private let nameField = FieldState()

    var body: some View {
        PanelScaffold(model: model, title: L.addSubscriptionTitle.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
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
                                Text(L.adding.tr(language))
                            }
                            .frame(maxWidth: .infinity)
                        } else {
                            Text(L.add.tr(language))
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .controlSize(.large)
                    .buttonStyle(.borderedProminent)
                    .disabled(!canAdd)

                    Text(L.subscriptionSavedEvenIfFetchFails.tr(language))
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                .padding(.horizontal, Metrics.rowPaddingH)
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
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
                .font(Typography.rowSubtitle)
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
            .font(Typography.rowValue)
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .padding(.vertical, 4)
    }
}
