// HomeView — the primary menu-bar panel.
//
// Layout follows a native macOS utility: an identity header, one prominent
// action, then plain rows. No cards, no large empty areas — a menu-bar panel
// has roughly 350×450 points to work with.
//
// Every clickable row is a Button with .buttonStyle(.plain), so hit targets
// come from SwiftUI rather than a hand-rolled overlay. That is the direct fix
// for the previous UI's "visible but not clickable" rows.

import SwiftUI

struct HomeView: View {
    let model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider()
            if let error = model.lastError {
                errorBanner(error)
            }
            if case .failed(let message) = model.connection {
                backendDownBanner(message)
            }
            primaryAction
            Divider()
            navigationRows
            Divider()
            footer
        }
        .padding(.vertical, 4)
    }

    // MARK: - Header

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 2) {
                Text("JiejieBox")
                    .font(.headline)
                StatusLine(state: model.core?.state, error: model.core?.error_message)
            }
            Spacer()
            if let version = model.core?.core_version, !version.isEmpty {
                Text(version)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    // MARK: - Primary action

    private var primaryAction: some View {
        VStack(spacing: 6) {
            Button {
                Task { await model.toggleCore() }
            } label: {
                HStack(spacing: 6) {
                    if let state = model.core?.state, state.isTransitioning {
                        ProgressView().controlSize(.small)
                    }
                    Text(primaryTitle)
                        .frame(maxWidth: .infinity)
                }
            }
            .controlSize(.large)
            .buttonStyle(.borderedProminent)
            .disabled(!canAct)
            .keyboardShortcut(.defaultAction)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 10)
    }

    /// Label for the primary button, driven purely by backend state.
    private var primaryTitle: String {
        guard let state = model.core?.state else { return "Start" }
        switch state {
        case .running: return "Stop"
        case .starting: return "Starting…"
        case .stopping: return "Stopping…"
        case .error: return "Retry"
        case .stopped: return "Start"
        }
    }

    /// Disabled while a transition is in flight — a second click during Start
    /// was one of the old UI's failure modes.
    private var canAct: Bool {
        guard case .ready = model.connection else { return false }
        guard let state = model.core?.state else { return false }
        return !state.isTransitioning
    }

    // MARK: - Navigation

    private var navigationRows: some View {
        VStack(spacing: 0) {
            NavigationRow(title: "Core Mode", value: coreModeLabel, systemImage: "gearshape") {
                model.path.append(.coreMode)
            }
            NavigationRow(title: "More", value: nil, systemImage: "ellipsis.circle") {
                model.path.append(.more)
            }
        }
    }

    private var coreModeLabel: String {
        (model.settings?.core_backend_mode ?? "classic").capitalized
    }

    // MARK: - Footer

    private var footer: some View {
        HStack {
            if let path = model.settings?.config_path {
                Button {
                    NSWorkspace.shared.selectFile(path, inFileViewerRootedAtPath: "")
                } label: {
                    Label("Reveal Config", systemImage: "doc")
                        .labelStyle(.titleAndIcon)
                }
                .buttonStyle(.plain)
                .help(path)
            }
            Spacer()
            Button("Quit") {
                Task {
                    await model.stopCore()
                    await model.stop()
                    NSApplication.shared.terminate(nil)
                }
            }
            .buttonStyle(.plain)
            .keyboardShortcut("q")
        }
        .font(.callout)
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    // MARK: - Banners

    private func errorBanner(_ message: String) -> some View {
        HStack(alignment: .top, spacing: 6) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
            Text(message)
                .font(.caption)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Button {
                model.clearError()
            } label: {
                Image(systemName: "xmark.circle.fill")
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private func backendDownBanner(_ message: String) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "bolt.slash")
                .foregroundStyle(.red)
            Text(message)
                .font(.caption)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Button("Restart") {
                Task { await model.restart() }
            }
            .controlSize(.small)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }
}

/// The status dot plus its label.
private struct StatusLine: View {
    let state: CoreState?
    let error: String?

    var body: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(color)
                .frame(width: 7, height: 7)
            Text(state?.label ?? "Connecting…")
                .font(.subheadline)
                .foregroundStyle(.secondary)
        }
        .help(error ?? "")
    }

    private var color: Color {
        switch state {
        case .running: return .green
        case .starting, .stopping: return .orange
        case .error: return .red
        default: return .secondary
        }
    }
}

/// A tappable navigation row: label, optional value, chevron.
///
/// A Button, not a Rectangle with a tap gesture — SwiftUI then owns the hit
/// target, focus and keyboard behaviour.
struct NavigationRow: View {
    let title: String
    let value: String?
    let systemImage: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 8) {
                Image(systemName: systemImage)
                    .frame(width: 16)
                    .foregroundStyle(.secondary)
                Text(title)
                Spacer()
                if let value {
                    Text(value)
                        .foregroundStyle(.secondary)
                }
                Image(systemName: "chevron.right")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            }
            .contentShape(Rectangle())
            .padding(.horizontal, 12)
            .padding(.vertical, 5)
        }
        .buttonStyle(.plain)
    }
}
