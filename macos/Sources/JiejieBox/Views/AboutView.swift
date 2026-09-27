// AboutView — version and links.

import SwiftUI

struct AboutView: View {
    let model: AppModel

    /// Project links. `URL(string:)` returns nil for a malformed string, so the
    /// result is optional and the views render conditionally rather than
    /// trapping.
    private static let githubURL = URL(string: "https://github.com/Piggy-Cat-bit-shadow/singbox-launcher")
    private static let telegramURL = URL(string: "https://t.me/singbox_launcher")

    private var frontendVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    }

    var body: some View {
        PanelScaffold(model: model, title: "About", onBack: { model.goBack() }) {
            VStack(spacing: 10) {
                Image(systemName: "bolt.horizontal.circle.fill")
                    .font(.system(size: 40))
                    .foregroundStyle(.tint)

                Text("JiejieBox")
                    .font(.title3.weight(.semibold))

                VStack(spacing: 2) {
                    Text("App \(frontendVersion)")
                    Text("Backend \(model.handshake?.backend_version ?? "—")")
                    Text("Protocol \(model.handshake?.protocol_version ?? 0)")
                    if let core = model.core?.core_version, !core.isEmpty {
                        Text("sing-box \(core)")
                    }
                }
                .font(.caption)
                .foregroundStyle(.secondary)

                // Explicitly typed constants rather than a force-unwrap. These
                // literals cannot fail today, but a crash point in a shipped view
                // earns nothing, and the optional is handled once here instead of
                // at every call site.
                HStack(spacing: 12) {
                    if let github = Self.githubURL {
                        Link("GitHub", destination: github)
                    }
                    if let telegram = Self.telegramURL {
                        Link("Telegram", destination: telegram)
                    }
                }
                .font(.callout)
            }
            .padding(20)
            .frame(maxWidth: .infinity)
        }
    }
}
