// DaemonPairView — paste the one-time invite the service printed.
//
// Pairing is the only step that needs a credential from the user, and the
// invite is the only one they ever handle: the backend parses it, enrols the
// client identity and stores the address and fingerprint. The long-lived secret
// never crosses the IPC boundary in either direction, so there is no secret
// field to get wrong.
//
// The copy names the Terminal command rather than the "Pair Service" button,
// because the button itself does not print anything — saying otherwise sent
// users looking for output that the UI never produced.

import SwiftUI

struct DaemonPairView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    private let inviteField = FieldState()

    var body: some View {
        PanelScaffold(model: model, title: L.pairService.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                MenuSection(L.pairing.tr(language)) {
                    LabeledField(label: L.inviteFormat.tr(language),
                                 placeholder: "address#fingerprint#code",
                                 state: inviteField)
                    MenuRow(L.pasteFromClipboard.tr(language), systemImage: "doc.on.clipboard") {
                        if let text = NSPasteboard.general.string(forType: .string) {
                            inviteField.text = text.trimmingCharacters(in: .whitespacesAndNewlines)
                        }
                    }
                    // The expected shape, stated up front. The fingerprint is
                    // never shown — only its position in the format — so this
                    // cannot leak a credential.
                    Text(L.inviteFormat.tr(language))
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .padding(.horizontal, Metrics.rowPaddingH)
                        .padding(.bottom, 4)
                }

                VStack(alignment: .leading, spacing: 6) {
                    Button {
                        pair()
                    } label: {
                        if model.pending == .pairingDaemon {
                            HStack(spacing: 6) {
                                ProgressView().controlSize(.small)
                                Text(L.pairing.tr(language))
                            }
                            .frame(maxWidth: .infinity)
                        } else {
                            Text(L.pair.tr(language))
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .controlSize(.large)
                    .buttonStyle(.borderedProminent)
                    .disabled(!canPair)

                    Text(L.pairInstructions.tr(language))
                        .font(Typography.rowSubtitle)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)

                    if needsFreshInvite {
                        Text(L.inviteOneTime.tr(language))
                            .font(Typography.rowSubtitle)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }

                    // A failed redeem is the moment a user needs a new invite,
                    // and going Back to hunt for Re-pair is exactly the loop
                    // this screen exists to remove.
                    Button {
                        Task {
                            model.clearDaemonCommand()
                            model.goBack()
                            await model.prepareDaemonPairing()
                        }
                    } label: {
                        Text(L.generateNewInvite.tr(language))
                    }
                    .controlSize(.small)
                    .disabled(model.pending != nil)
                }
                .padding(.horizontal, Metrics.rowPaddingH)
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
        }
        // A successful pair returns to the Daemon screen, which now shows the
        // updated status — the point of pairing is to change that page.
        .onChange(of: model.daemon?.paired) { _, paired in
            if paired == true { model.goBack() }
        }
    }

    /// True once a pair attempt has failed, which is when a fresh invite is
    /// likely needed.
    private var needsFreshInvite: Bool {
        model.lastError != nil
    }

    private var canPair: Bool {
        guard model.pending == nil else { return false }
        // The backend owns the real validation; this only keeps the button
        // honest about obvious non-invites, matching the same shape check the
        // backend applies so the two cannot disagree.
        let trimmed = inviteField.text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return false }
        return trimmed.filter { $0 == "#" }.count >= 2
    }

    private func pair() {
        let invite = inviteField.text.trimmingCharacters(in: .whitespacesAndNewlines)
        // No navigation on failure: the user stays here with the error visible
        // and the invite still in the field, so a typo can be corrected.
        Task { _ = await model.pairDaemon(invite: invite) }
    }
}
