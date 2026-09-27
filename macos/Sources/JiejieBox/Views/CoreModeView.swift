// CoreModeView — shows how the core is run (classic child process vs daemon).
//
// Display-only. Switching is not implemented: the backend exposes no
// set_core_mode method, and offering a control that cannot act would be the
// "looks clickable, does nothing" pattern the cutdown audit forbids. The rows
// are therefore plain, non-interactive content with an explicit note, rather
// than a picker that silently discards the choice.

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
                            Text("Active")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            } footer: {
                Text("Changing the mode is not available yet.")
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
