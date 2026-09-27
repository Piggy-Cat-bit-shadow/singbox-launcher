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
        ScrollView {
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
                    pathRow("Config file", model.settings?.config_path)
                    pathRow("Data", model.settings?.data_dir)
                    pathRow("Logs", model.settings?.logs_dir)
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

    /// A path row that reveals the location on click.
    private func pathRow(_ title: String, _ path: String?) -> some View {
        let usable = !(path ?? "").isEmpty
        return MenuRow(title, systemImage: "folder",
                       value: usable ? abbreviate(path!) : "Unknown",
                       action: {
                           guard let path, !path.isEmpty else { return }
                           NSWorkspace.shared.selectFile(path, inFileViewerRootedAtPath: "")
                       })
            .disabled(!usable)
            .help(path ?? "Unknown")
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
