// CoreDetailsView — runtime diagnostics.
//
// This replaces the old full Diagnostics page. It shows the facts that matter
// when something is wrong, without a second dashboard: status, versions, the
// resolved paths, and the backend identity.
//
// Missing values are shown as "Unknown"/"Missing" rather than omitted, so a
// blank row is never ambiguous between "no data" and "not implemented".

import SwiftUI

struct CoreDetailsView: View {
    let model: AppModel

    var body: some View {
        PanelScaffold(model: model, title: "Core Details", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                MenuSection("Core") {
                    detail("Status", model.core?.state.label)
                    detail("Version", model.core?.core_version, fallback: "Unknown")
                    detail("Mode", model.coreModeLabel)
                    detail("Binary", model.coreMissing ? "Missing" : "Found")
                    detail("Config", model.configMissing ? "Missing" : "Ready")
                }

                MenuSection("Backend") {
                    detail("Version", model.handshake?.backend_version, fallback: "Unknown")
                    detail("Protocol", "\(model.handshake?.protocol_version ?? 0)")
                    detail("Process ID", model.handshake.map { String($0.pid) }, fallback: "—")
                }

                MenuSection("Paths") {
                    pathRow("Config file", model.settings?.config_path, kind: .file)
                    pathRow("Data", model.settings?.data_dir, kind: .directory)
                    pathRow("Logs", model.settings?.logs_dir, kind: .directory)
                }

                MenuSection {
                    MenuRow("Restart Core",
                            subtitle: "Stop and start the core again",
                            systemImage: "arrow.clockwise") {
                        Task { await model.restartCore() }
                    }
                    if model.pending == .restarting {
                        pendingRow("Restarting…")
                    }
                }
            }
            .padding(.vertical, 8)
        }
    }

    /// A read-only detail row.
    private func detail(_ title: String, _ value: String? = nil, fallback: String = "—") -> some View {
        HStack(spacing: 8) {
            Text(title)
            Spacer(minLength: 8)
            Text(value?.isEmpty == false ? value! : fallback)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
                .help(value ?? fallback)
        }
        .frame(minHeight: Metrics.rowHeight)
        .padding(.horizontal, Metrics.rowPaddingH)
    }

    /// What a path row points at, because a file and a directory are opened
    /// differently. Treating all three paths as files meant "Data" and "Logs"
    /// revealed a folder's parent with the folder selected, rather than opening
    /// the folder — the same click doing two different things depending on the
    /// row, with nothing to tell the user which was which.
    enum PathKind {
        case file
        case directory
    }

    /// A path row that reveals the location on click.
    ///
    /// The row is disabled when the path is unknown, and the help text says
    /// which action it performs, so what a click will do is never a surprise.
    private func pathRow(_ title: String, _ path: String?, kind: PathKind) -> some View {
        let usable = !(path ?? "").isEmpty
        let action = kind == .file ? "Reveal in Finder" : "Open Folder"
        return MenuRow(title,
                       subtitle: usable ? action : "Path not reported",
                       systemImage: kind == .file ? "doc" : "folder",
                       value: usable ? abbreviate(path!) : "Unknown",
                       action: {
                           guard let path, !path.isEmpty else { return }
                           switch kind {
                           case .file:
                               NSWorkspace.shared.selectFile(path, inFileViewerRootedAtPath: "")
                           case .directory:
                               NSWorkspace.shared.open(URL(fileURLWithPath: path))
                           }
                       })
            .disabled(!usable)
            .help(usable ? "\(action): \(path!)" : "The backend did not report this path.")
    }

    private func pendingRow(_ text: String) -> some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(text).font(.caption).foregroundStyle(.secondary)
            Spacer()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .frame(minHeight: 24)
    }

    /// Shorten a long path for display; the full value is in the tooltip.
    private func abbreviate(_ path: String) -> String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        if path.hasPrefix(home) {
            return "~" + path.dropFirst(home.count)
        }
        return path
    }
}
