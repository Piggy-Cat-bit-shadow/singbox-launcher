// CoreModeView — choose how the core is run (classic child process vs daemon).
//
// Read-only for now: the backend reports the active mode in the snapshot, and
// switching it is a core-lifecycle operation that belongs with the rest of the
// runtime work rather than being faked here.

import SwiftUI

struct CoreModeView: View {
    let model: AppModel

    var body: some View {
        Form {
            Section {
                ForEach(modes, id: \.id) { mode in
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(mode.title)
                            Text(mode.detail)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                        if mode.id == activeID {
                            Image(systemName: "checkmark")
                                .foregroundStyle(.tint)
                        }
                    }
                    .contentShape(Rectangle())
                }
            } footer: {
                Text("The mode is stored with your settings and applied on the next core start.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
    }

    private var activeID: String {
        model.settings?.core_backend_mode ?? "classic"
    }

    private var modes: [(id: String, title: String, detail: String)] {
        [
            ("classic", "Classic", "The launcher runs sing-box itself."),
            ("daemon", "Daemon", "A system service runs the core and keeps it alive."),
        ]
    }
}
