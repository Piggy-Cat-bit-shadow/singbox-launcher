// MoreView — secondary actions and the few real settings.
//
// Grouped by what the user is trying to do, not by implementation: files are
// opened with NSWorkspace (a frontend job), while the automation toggles are
// business settings the backend owns and persists.

import SwiftUI

struct MoreView: View {
    let model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            MenuSection("Core") {
                MenuRow("Restart Core", systemImage: "arrow.clockwise") {
                    Task { await model.restartCore() }
                }
                if model.pending == .restarting {
                    PendingRow("Restarting…")
                }
            }

            MenuSection("Automation") {
                toggleRow("Auto Ping After Connect",
                          isOn: model.settings?.auto_ping_after_connect ?? false,
                          help: "Test proxies shortly after the core connects.") { value in
                    Task { await model.setAutoPing(value) }
                }
                toggleRow("Auto Update Subscriptions",
                          isOn: model.settings?.auto_update_subscriptions ?? false,
                          help: "Refresh subscription data on a schedule.") { value in
                    Task { await model.setAutoUpdateSubscriptions(value) }
                }
                toggleRow("Launch at Login",
                          isOn: model.launchAtLogin,
                          help: "Start JiejieBox when you sign in.") { value in
                    model.setLaunchAtLogin(value)
                }
            }

            MenuSection("Configuration") {
                MenuRow("Reload Config", systemImage: "arrow.triangle.2.circlepath") {
                    Task { await model.reloadConfig() }
                }
                MenuRow("Update Subscriptions", systemImage: "arrow.down.circle") {
                    Task { await model.updateSubscriptions() }
                }
                switch model.pending {
                case .reloadingConfig: PendingRow("Rebuilding config.json…")
                case .updatingSubscriptions: PendingRow("Updating subscriptions…")
                default: EmptyView()
                }
            }

            MenuSection("Files") {
                MenuRow("Open Config", systemImage: "doc") { model.revealConfig() }
                    .disabled(model.settings?.config_path.isEmpty ?? true)
                MenuRow("Open Config Folder", systemImage: "folder") {
                    model.openConfigFolder()
                }
                .disabled(model.settings?.data_dir.isEmpty ?? true)
                MenuRow("Open Logs", systemImage: "text.alignleft") { model.openLogs() }
                    .disabled(model.settings?.logs_dir.isEmpty ?? true)
            }

            MenuSection {
                MenuRow("About JiejieBox", systemImage: "info.circle", showsChevron: true) {
                    model.path.append(.about)
                }
            }
        }
        .padding(.vertical, 8)
    }

    /// A settings toggle presented as a full-width row.
    ///
    /// Built from MenuRow plus a Switch so the whole row is the hit target and
    /// the label belongs to the control, rather than a bare Switch the user
    /// has to aim at precisely.
    private func toggleRow(_ title: String, isOn: Bool, help: String,
                           set: @escaping (Bool) -> Void) -> some View {
        MenuRow(title, action: { set(!isOn) },
                trailing: {
                    Toggle("", isOn: Binding(get: { isOn }, set: set))
                        .labelsHidden()
                        .toggleStyle(.switch)
                        .controlSize(.small)
                        .allowsHitTesting(false)
                })
            .help(help)
    }
}

/// A row showing that an operation is in flight.
struct PendingRow: View {
    let text: String

    init(_ text: String) { self.text = text }

    var body: some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(text).font(.caption).foregroundStyle(.secondary)
            Spacer()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .frame(minHeight: 24)
    }
}
