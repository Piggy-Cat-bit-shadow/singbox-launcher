// swift-tools-version: 5.9
//
// JiejieBox — native macOS menu-bar frontend.
//
// Built with SwiftPM rather than an Xcode project: the build machine has only
// Command Line Tools, where xcodebuild refuses to run. SwiftPM produces the
// same arm64 SwiftUI executable, and build/build_macos_app.sh assembles it
// into the .app bundle alongside the Go helper.
import PackageDescription

let package = Package(
    name: "JiejieBox",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "JiejieBox",
            path: "Sources/JiejieBox"
        )
        // No test target: this toolchain ships neither XCTest nor the Swift
        // Testing macro plugin, so `swift test` cannot build here at all
        // (verified). Swift-side invariants that can be checked textually are
        // enforced from the Go suite instead, which does run — see
        // backend/service/contract_test.go, TestSwiftTimeoutBudgets.
    ]
)
