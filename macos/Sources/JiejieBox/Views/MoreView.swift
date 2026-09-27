// MoreView — the secondary actions and the few real settings.
//
// Everything here either opens a file/folder through NSWorkspace (a frontend
// responsibility) or is a preference the backend owns.

import SwiftUI
import ServiceManagement

struct MoreView: View {
    let model: AppModel
    /// Launch-at-login state lives on the model so the view stays free of
    /// SwiftUI property wrappers (unavailable in this toolchain).

    var body: some View {
        Form {
            Section("Files") {
                actionRow("Open Config", systemImage: "doc") {
                    if let path = model.settings?.config_path {
                        NSWorkspace.shared.open(URL(fileURLWithPath: path))
                    }
                }
                actionRow("Open Config Folder", systemImage: "folder") {
                    if let dir = model.settings?.data_dir {
                        NSWorkspace.shared.open(URL(fileURLWithPath: dir))
                    }
                }
                actionRow("Open Logs", systemImage: "text.alignleft") {
                    // The backend reports the log directory; Swift must not
                    // guess it, or the button silently points nowhere when the
                    // layout changes.
                    if let logs = model.settings?.logs_dir, !logs.isEmpty {
                        NSWorkspace.shared.open(URL(fileURLWithPath: logs))
                    }
                }
                .disabled(model.settings?.logs_dir.isEmpty ?? true)
            }

            Section("Startup") {
                Toggle("Launch at Login", isOn: Binding(
                    get: { model.launchAtLogin },
                    set: { model.setLaunchAtLogin($0) }
                ))
            }

            Section("Appearance") {
                Picker("Appearance", selection: Binding(
                    get: { model.appearance },
                    set: { model.appearance = $0 }
                )) {
                    ForEach(AppModel.AppearancePreference.allCases) { pref in
                        Text(pref.label).tag(pref)
                    }
                }
                .pickerStyle(.segmented)
            }

            Section {
                actionRow("About JiejieBox", systemImage: "info.circle") {
                    model.path.append(.about)
                }
            }
        }
        .formStyle(.grouped)
    }

    private func actionRow(_ title: String, systemImage: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack {
                Label(title, systemImage: systemImage)
                Spacer()
                Image(systemName: "chevron.right")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

}
