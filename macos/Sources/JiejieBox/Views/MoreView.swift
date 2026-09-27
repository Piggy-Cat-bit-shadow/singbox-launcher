// MoreView — settings and secondary navigation.
//
// Information architecture, in the order a user actually looks for things:
//
//   Automation   what the app does on its own (toggles)
//   Application  how the app itself behaves (appearance, language)
//   Files        where the app's files are (actions)
//   About        what the app is
//
// The grouping is by KIND, not by subsystem: two toggles belong together
// because they are toggles, and Appearance and Language belong together because
// both are "how the app presents itself". The earlier layout put Appearance in a
// section by itself, which made it look like an afterthought and gave the page
// four unrelated shapes of row.
//
// Configuration and Core used to live here too. They moved out because they are
// not settings: Configuration is about the generated config file and Core is
// about the running process, and both are reachable from where they are actually
// used — the Subscriptions screen offers Reload, and Core Details owns Restart.
// Leaving them here produced a page where "Open Logs" and "Restart Core" sat
// side by side with no relationship.
//
// Quit is in the PanelScaffold header on every screen, so it is deliberately
// absent here: two quit controls on one panel would be a duplicate.

import SwiftUI

struct MoreView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    var body: some View {
        PanelScaffold(model: model, title: L.more.tr(language),
                      onBack: { model.goBack() }) {
            VStack(alignment: .leading, spacing: Metrics.groupSpacing) {
                automationSection
                applicationSection
                filesSection
                aboutSection
            }
            .padding(.top, Metrics.contentTopPadding)
            .padding(.bottom, Metrics.contentBottomPadding)
        }
        .task {
            if model.subscriptions.isEmpty { await model.loadSubscriptions() }
        }
    }

    // MARK: - Automation

    /// Two toggles, identical in weight and height.
    ///
    /// Both are standard toggle rows. Auto Ping and Auto Update are backend
    /// settings with per-row pending state; Launch at Login is frontend-only
    /// (SMAppService), so it has no pending state to show — it reports failure
    /// through the error banner instead. That difference is invisible here
    /// because both render through the same row primitive.
    private var automationSection: some View {
        MenuSection(L.automation.tr(language)) {
            MenuToggleRow(
                title: L.autoUpdateSubscriptions.tr(language),
                subtitle: L.autoUpdateHelp.tr(language),
                systemImage: "arrow.triangle.2.circlepath",
                isOn: model.settings?.auto_update_subscriptions ?? false,
                pending: model.pending == .updatingSetting(.autoUpdateSubscriptions),
                disabled: model.pending != nil
            ) { value in
                Task { await model.setAutoUpdateSubscriptions(value) }
            }

            MenuToggleRow(
                title: L.autoPingAfterConnect.tr(language),
                subtitle: L.autoPingHelp.tr(language),
                systemImage: "speedometer",
                isOn: model.settings?.auto_ping_after_connect ?? false,
                pending: model.pending == .updatingSetting(.autoPing),
                disabled: model.pending != nil
            ) { value in
                Task { await model.setAutoPing(value) }
            }

            MenuToggleRow(
                title: L.launchAtLogin.tr(language),
                subtitle: L.launchAtLoginHelp.tr(language),
                systemImage: "power",
                isOn: model.launchAtLogin,
                // No pending state: SMAppService is a local, synchronous call.
                pending: false,
                // Deliberately NOT disabled during backend operations — it does
                // not touch the backend, so blocking it would be a lie about
                // what it depends on.
                disabled: false
            ) { value in
                model.setLaunchAtLogin(value)
            }
        }
    }

    // MARK: - Application

    /// Appearance and Language as one visually consistent pair.
    ///
    /// Both are `MenuPickerRow`s: the row IS the menu, the current value shows on
    /// the right, and there is no navigation to a second screen. That keeps them
    /// at the same level as the toggles above (a menu row and a toggle row are
    /// the same shape, differing only in the trailing control), so the section
    /// reads as one group rather than a header with an orphan under it.
    private var applicationSection: some View {
        MenuSection(L.application.tr(language)) {
            // Frontend-only preference: never reaches the backend, so
            // UserDefaults stays the single source of truth.
            MenuPickerRow(
                title: L.appearance.tr(language),
                systemImage: "circle.lefthalf.filled",
                options: AppModel.AppearancePreference.allCases,
                selection: model.appearance,
                label: { $0.label(language) },
                onSelect: { model.appearance = $0 })

            MenuPickerRow(
                title: L.language.tr(language),
                systemImage: "globe",
                options: AppLanguage.allCases,
                selection: model.language.preference,
                label: { $0.label(language) },
                onSelect: { model.language.preference = $0 })
        }
    }

    // MARK: - Files

    /// Three actions on the app's own files, as one group.
    ///
    /// Each is disabled when its path is unknown rather than opening an empty
    /// Finder window: a control that visibly cannot do anything is clearer than
    /// one that appears to work and shows nothing.
    private var filesSection: some View {
        MenuSection(L.files.tr(language)) {
            MenuRow(L.openConfig.tr(language), systemImage: "doc") {
                model.revealConfig()
            }
            .disabled(model.settings?.config_path.isEmpty ?? true)

            MenuRow(L.openConfigFolder.tr(language), systemImage: "folder") {
                model.openConfigFolder()
            }
            .disabled(model.settings?.data_dir.isEmpty ?? true)

            MenuRow(L.openLogs.tr(language), systemImage: "text.alignleft") {
                model.openLogs()
            }
            .disabled(model.settings?.logs_dir.isEmpty ?? true)
        }
    }

    // MARK: - About

    /// The About entry, in its own section so it does not read as a stray row.
    private var aboutSection: some View {
        MenuSection {
            MenuRow(L.aboutJiejieBox.tr(language),
                    systemImage: "info.circle", showsChevron: true) {
                model.path.append(.about)
            }
            MenuRow(L.subscriptions.tr(language),
                    systemImage: "arrow.down.circle",
                    value: subscriptionCount, showsChevron: true) {
                model.path.append(.subscriptions)
            }
        }
    }

    private var subscriptionCount: String {
        let count = model.subscriptions.count
        return count == 0 ? L.none.tr(language) : "\(count)"
    }
}

/// A row showing that an operation is in flight.
struct PendingRow: View {
    let text: String

    init(_ text: String) { self.text = text }

    var body: some View {
        HStack(spacing: 6) {
            ProgressView().controlSize(.small)
            Text(text)
                .font(Typography.status)
                .foregroundStyle(.secondary)
                .lineLimit(1)
            Spacer()
        }
        .padding(.horizontal, Metrics.rowPaddingH)
        .frame(minHeight: 24)
    }
}
