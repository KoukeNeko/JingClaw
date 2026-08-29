// swift-tools-version: 6.0
import PackageDescription

// The macOS client.
//
// JingClawKit is everything that is not a view: the reducer three clients
// share, the models it works on, and the connection to the daemon. Separate
// from the app so the behaviour can be tested without a window, and so the
// fixtures every client is checked against can be run by `swift test`.
let package = Package(
    name: "JingClaw",
    platforms: [.macOS(.v14)],
    products: [
        .library(name: "JingClawKit", targets: ["JingClawKit"]),
        .executable(name: "JingClaw", targets: ["JingClawApp"]),
    ],
    targets: [
        .target(name: "JingClawKit"),
        .executableTarget(name: "JingClawApp", dependencies: ["JingClawKit"]),
        .testTarget(
            name: "JingClawKitTests",
            dependencies: ["JingClawKit"],
            path: "Tests/JingClawKitTests"
        )
    ]
)
