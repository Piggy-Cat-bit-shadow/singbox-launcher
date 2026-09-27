// AboutView — version and links.

import SwiftUI

struct AboutView: View {
    let model: AppModel
    @Environment(\.localization) private var language

    /// Project links. `URL(string:)` returns nil for a malformed string, so the
    /// result is optional and the views render conditionally rather than
    /// trapping.
    private static let githubURL = URL(string: "https://github.com/Piggy-Cat-bit-shadow/singbox-launcher")
    private static let telegramURL = URL(string: "https://t.me/singbox_launcher")

    private var frontendVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    }

    var body: some View {
        PanelScaffold(model: model, title: L.about.tr(language),
                      onBack: { model.goBack() }) {
            VStack(spacing: 10) {
                Image(systemName: "bolt.horizontal.circle.fill")
                    .font(Typography.heroGlyph)
                    .foregroundStyle(.tint)

                Text(L.appName.tr(language))
                    .font(Typography.productName)

                Text(L.aboutBlurb.tr(language))
                    .font(Typography.rowSubtitle)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)

                VStack(spacing: 2) {
                    Text("\(L.appVersion.tr(language)) \(frontendVersion)")
                    Text("\(L.backendVersion.tr(language)) \(model.handshake?.backend_version ?? "—")")
                    Text("\(L.protocolVersion.tr(language)) \(model.handshake?.protocol_version ?? 0)")
                    if let core = model.core?.core_version, !core.isEmpty {
                        Text("sing-box \(core)")
                    }
                }
                .font(Typography.status)
                .foregroundStyle(.secondary)

                // Explicitly typed constants rather than a force-unwrap. These
                // literals cannot fail today, but a crash point in a shipped view
                // earns nothing, and the optional is handled once here instead of
                // at every call site.
                HStack(spacing: 12) {
                    if let github = Self.githubURL {
                        Link(L.github.tr(language), destination: github)
                    }
                    if let telegram = Self.telegramURL {
                        Link(L.telegram.tr(language), destination: telegram)
                    }
                }
                .font(Typography.rowValue)
            }
            .padding(20)
            .frame(maxWidth: .infinity)
        }
    }
}
