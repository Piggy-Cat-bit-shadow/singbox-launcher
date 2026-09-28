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

    /// Owned by the model so a re-render cannot discard a pasted invite — the
    /// value the user is most likely to lose and least likely to have kept a
    /// copy of.
    private var inviteField: TextDraft { model.drafts.draft(DraftStore.daemonInvite) }

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

                    if pairAttemptFailed {
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
                            model.popIfCurrent(.daemonPair)
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
        // NAVIGATION IS DRIVEN BY THE ATTEMPT, NOT BY THE FLAG.
        //
        // This was `.onChange(of: model.daemon?.paired) { if paired { goBack() } }`,
        // which silently does nothing on the one path that matters most: RE-PAIRING.
        // The daemon is already paired when the screen opens, so a successful
        // re-pair produces no value CHANGE — `true` to `true` — and the screen
        // stayed put while the user waited for a return that never came.
        //
        // The attempt's own result is the correct trigger, and `pairDaemon`
        // already returns it. Using it also means the pop is owned by the
        // operation rather than by an unrelated state observation, which is what
        // let a user who pressed Back mid-pair be popped a second time.
    }

    /// True once a PAIR attempt has failed, which is when a fresh invite is
    /// likely needed.
    ///
    /// Previously `model.lastError != nil`, which is the app-wide error line:
    /// any unrelated failure (a proxy switch, a settings save) left this warning
    /// on screen the moment the Pair page was opened, telling the user their
    /// invite was spent when they had not tried one. The signal has to belong to
    /// this attempt, so it is recorded when this screen's pair call fails.
    private var pairAttemptFailed: Bool { model.lastPairAttemptFailed == true }

    private var canPair: Bool {
        guard model.pending == nil else { return false }
        // The backend owns the real validation; this only keeps the button
        // honest about obvious non-invites, matching the same shape check the
        // backend applies so the two cannot disagree.
        let trimmed = inviteField.trimmed
        guard !trimmed.isEmpty else { return false }
        return trimmed.filter { $0 == "#" }.count >= 2
    }

    private func pair() {
        let invite = inviteField.trimmed
        // No navigation on failure: the user stays here with the error visible
        // and the invite still in the field, so a typo can be corrected.
        Task {
            let paired = await model.pairDaemon(invite: invite)
            if paired {
                // The invite has been redeemed, so the draft must not be restored
                // if the user opens this screen again.
                model.drafts.clear(DraftStore.daemonInvite)
                // Leave only if the user is STILL here. Without this check,
                // pressing Back while the pair was in flight let the completion
                // pop a second screen — the user's manual exit plus the
                // operation's automatic one, moving two levels at once.
                model.popIfCurrent(.daemonPair)
            }
        }
    }
}
