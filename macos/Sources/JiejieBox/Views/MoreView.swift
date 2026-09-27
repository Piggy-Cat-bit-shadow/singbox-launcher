// MoreView — secondary actions and the few real settings.
//
// Subscriptions now has its own screen and its own entry on Home, so this page
// no longer carries a lone "Update Subscriptions" button with no context: the
// list, the add button and the update action all live together where the user
// can see what "update" refers to.
//
// Quit is in the PanelScaffold header on every screen, so it is deliberately
// absent here — two quit controls on one panel would be a duplicate, not a
// convenience.

import SwiftUI

struct MoreView: View {
    let model: AppModel

    var body: some View {
        PanelScaffold(model: model, title: "More", onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: 10) {
                MenuSection("Configuration") {
                    MenuRow("Subscriptions", systemImage: "arrow.down.circle",
                            value: subscriptionCount,
                            showsChevron: true) {
                        model.path.append(.subscriptions)
                    }
                    MenuRow("Reload Config", systemImage: "arrow.triangle.2.circlepath") {
                        Task {
                            await model.reloadConfig()
                            await model.refreshCoreState()
                        }
                    }
                    .disabled(model.pending != nil)
                    if model.pending == .reloadingConfig {
                        PendingRow("Rebuilding config.json…")
                    }
                }

                MenuSection("Core") {
                    MenuRow("Restart Core", systemImage: "arrow.clockwise") {
                        Task { await model.restartCore() }
                    }
                    .disabled(model.pending != nil)
                    if model.pending == .restarting {
                        PendingRow("Restarting…")
                    }
                }

                MenuSection("Automation") {
                    toggleRow("Auto Ping After Connect",
                              isOn: model.settings?.auto_ping_after_connect ?? false,
                              help: "Test proxies shortly after the core connects.",
                              id: .autoPing) { value in
                        Task { await model.setAutoPing(value) }
                    }
                    toggleRow("Auto Update Subscriptions",
                              isOn: model.settings?.auto_update_subscriptions ?? false,
                              help: "Refresh subscription data on a schedule.",
                              id: .autoUpdateSubscriptions) { value in
                        Task { await model.setAutoUpdateSubscriptions(value) }
                    }
                    // Launch at Login is frontend-only (SMAppService): the call
                    // is synchronous and reports failure through the error
                    // banner, so it has no backend pending state to show.
                    frontendToggleRow("Launch at Login",
                                      isOn: model.launchAtLogin,
                                      help: "Start JiejieBox when you sign in.") { value in
                        model.setLaunchAtLogin(value)
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
        .task {
            if model.subscriptions.isEmpty { await model.loadSubscriptions() }
        }
    }

    private var subscriptionCount: String {
        let count = model.subscriptions.count
        if count == 0 { return "None" }
        return "\(count)"
    }

    /// A settings toggle presented as a full-width row.
    ///
    /// Built from MenuRow plus a switch, so the whole row is the hit target and
    /// the label belongs to the control rather than a bare switch the user has
    /// to aim at precisely.
    /// A frontend-only toggle: no backend call, so no pending state.
    ///
    /// Separate from `toggleRow` because it must NOT block on, or be blocked
    /// by, backend operations — SMAppService is a local system call.
    private func frontendToggleRow(_ title: String, isOn: Bool, help: String,
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

    /// A boolean setting as a full-width row.
    ///
    /// The switch is drawn rather than interactive: the whole row is the hit
    /// target (a bare switch is a small target, and a row is not). `id` lets
    /// the row show its OWN saving state, so a user who flips two settings can
    /// see which one is still in flight instead of both looking stuck.
    private func toggleRow(_ title: String, isOn: Bool, help: String,
                           id: AppModel.SettingID,
                           set: @escaping (Bool) -> Void) -> some View {
        let saving = model.pending == .updatingSetting(id)
        return MenuRow(title,
                       subtitle: saving ? "Saving…" : nil,
                       action: { set(!isOn) },
                       trailing: {
                           if saving {
                               ProgressView().controlSize(.small)
                           } else {
                               Toggle("", isOn: Binding(get: { isOn }, set: set))
                                   .labelsHidden()
                                   .toggleStyle(.switch)
                                   .controlSize(.small)
                                   .allowsHitTesting(false)
                           }
                       })
            .help(help)
            .disabled(model.pending != nil)
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
