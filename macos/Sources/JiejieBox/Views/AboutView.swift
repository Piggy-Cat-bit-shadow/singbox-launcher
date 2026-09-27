// AboutView — version and links.

import SwiftUI

struct AboutView: View {
    let model: AppModel

    private var frontendVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    }

    var body: some View {
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

            HStack(spacing: 12) {
                Link("GitHub", destination: URL(string: "https://github.com/Piggy-Cat-bit-shadow/singbox-launcher")!)
                Link("Telegram", destination: URL(string: "https://t.me/singbox_launcher")!)
            }
            .font(.callout)
        }
        .padding(20)
        .frame(maxWidth: .infinity)
    }
}
