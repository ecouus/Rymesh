// swift-tools-version:5.9
// The macOS app: a native shell (menu bar, window, notifications) around the
// Go console bundled inside it. Built with SwiftPM, packaged by build-app.sh.
import PackageDescription

let package = Package(
    name: "RymeshMac",
    platforms: [.macOS(.v13)],
    targets: [
        // tools-version 5.9 builds in the Swift 5 language mode.
        .executableTarget(name: "RymeshMac", path: "Sources/RymeshMac")
    ]
)
