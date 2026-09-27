// DaemonPairView — paste the invite the service printed.
//
// Pairing is the only step that needs a credential from the user, and the
// invite is the only one they ever handle: the backend parses it, enrols the
// client identity and stores the address and fingerprint. The long-lived secret
// never crosses the IPC boundary in either direction, so there is no secret
// field to get wrong.

import SwiftUI

struct DaemonPairView: View {
    let model: AppModel

    private let inviteField = FieldState()

    var body: some View {
        PanelScaffold(model: model, title: "Pair Daemon", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                MenuSection("Invite") {
                    LabeledField(label: "Invite",
                                 placeholder: "address#fingerprint#code",
                                 state: inviteField)
                    MenuRow("Paste from Clipboard", systemImage: "doc.on.clipboard") {
                        if let text = NSPasteboard.general.string(forType: .string) {
                            inviteField.text = text.trimmingCharacters(in: .whitespacesAndNewlines)
                        }
                    }
                }

                VStack(alignment: .leading, spacing: 6) {
                    Button {
                        pair()
                    } label: {
                        if model.pending == .pairingDaemon {
                            HStack(spacing: 6) {
                                ProgressView().controlSize(.small)
                                Text("Pairing…")
                            }
                            .frame(maxWidth: .infinity)
                        } else {
                            Text("Pair")
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .controlSize(.large)
                    .buttonStyle(.borderedProminent)
                    .disabled(!canPair)

                    Text("Run “Pair Service” on the Daemon screen first: the command prints "
                         + "a one-time invite.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .padding(.horizontal, Metrics.rowPaddingH)
            }
            .padding(.vertical, 10)
        }
        // A successful pair returns to the Daemon screen, which now shows the
        // updated status — the point of pairing is to change that page.
        .onChange(of: model.daemon?.paired) { _, paired in
            if paired == true { model.goBack() }
        }
    }

    private var canPair: Bool {
        guard model.pending == nil else { return false }
        // The backend enforces the address#fingerprint#code shape; mirroring the
        // check here keeps the button honest without duplicating the parser.
        return inviteField.text.filter { $0 == "#" }.count >= 2
    }

    private func pair() {
        let invite = inviteField.text.trimmingCharacters(in: .whitespacesAndNewlines)
        Task { _ = await model.pairDaemon(invite: invite) }
    }
}
