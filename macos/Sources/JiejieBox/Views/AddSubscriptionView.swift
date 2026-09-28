// AddSubscriptionView — enter a URL, optionally name it.
//
// Two fields only. The URL is required; the name is not, because forcing a
// display name on someone who just wants to paste a link is friction, and the
// provider's own profile title replaces it after the first fetch anyway.

import SwiftUI

struct AddSubscriptionView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    // The drafts come from the MODEL, never from a stored property here.
    //
    // A `private let urlField = FieldState()` on a View struct is replaced every
    // time the parent body runs, so a model update arriving while the user types
    // could hand this body a fresh, empty draft and the URL would vanish. The
    // model owns them and hands back the same instance by key — see DraftStore.
    private var urlField: TextDraft { model.drafts.draft(DraftStore.addSubscriptionURL) }
    private var nameField: TextDraft { model.drafts.draft(DraftStore.addSubscriptionName) }

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

                    Text(L.subscriptionSavedNotYetFetched.tr(language))
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
    ///
    /// Case-insensitively, because the backend lowercases before checking
    /// (`strings.ToLower` in `looksLikeURL`). The previous case-sensitive prefix
    /// test meant `HTTPS://example.com/sub` was ACCEPTED by the backend but left
    /// the Add button permanently disabled: the user could see a valid URL in the
    /// field and had no way to submit it.
    ///
    /// Whitespace is trimmed with the same set the backend uses, so trailing
    /// newlines from a paste do not disable the button either.
    private var canAdd: Bool {
        guard model.pending == nil else { return false }
        return SubscriptionURLInput.looksValid(urlField.text)
    }

    private func add() {
        let url = urlField.trimmed
        let name = nameField.trimmed
        Task {
            // Pop only on success: leaving the form open on failure keeps the
            // user's typed URL available to correct, and the draft is owned by
            // the model so it genuinely survives the re-render that the failure
            // causes.
            if await model.addSubscription(name: name, url: url) {
                // The work is done, so the draft must not be restored next time.
                model.drafts.clear(DraftStore.addSubscriptionURL)
                model.drafts.clear(DraftStore.addSubscriptionName)
                // Only if the user is STILL here: pressing Back while the add was
                // in flight must not be followed by a second, automatic pop.
                model.popIfCurrent(.addSubscription)
            }
        }
    }
}

/// A labelled text field sized for the panel.
struct LabeledField: View {
    let label: String
    var placeholder: String = ""
    let state: TextDraft
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
