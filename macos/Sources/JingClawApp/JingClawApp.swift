import AppKit
import JingClawKit
import SwiftUI

@main
struct JingClawApp: App {
    @State private var launch = Launch()

    var body: some Scene {
        WindowGroup("JingClaw") {
            switch launch.phase {
            case .looking:
                ProgressView("Looking for the daemon…")
                    .frame(minWidth: 720, minHeight: 480)
                    .task { await launch.connect() }

            case .connected(let store, let gateway):
                SessionView(store: store, gateway: gateway)
                    .frame(minWidth: 820, minHeight: 520)

            case .failed(let reason):
                // Said rather than shown as an empty window. The usual cause
                // is that no daemon is running, and the fix is a command.
                NotRunningView(
                    reason: reason,
                    project: launch.project,
                    choose: { await launch.chooseProject() },
                    retry: { await launch.connect() }
                )
                .frame(minWidth: 560, minHeight: 360)
            }
        }
    }
}

@Observable
@MainActor
final class Launch {
    enum Phase {
        case looking
        case connected(SessionStore, GatewayStore)
        case failed(String)
    }

    var phase: Phase = .looking

    /// The project whose deployment this window is for.
    ///
    /// An application launched from Finder has a working directory of "/", so
    /// walking up from it finds nothing: the search that works for a command
    /// cannot work here. The folder is chosen once and remembered.
    /// The project folder, and the permission to read it.
    ///
    /// Kept as a bookmark rather than a path. macOS grants an application
    /// access to Documents, Desktop and Downloads through the act of choosing
    /// a folder, and a path remembered without that grant is a path the app
    /// is still refused — which looks exactly like no daemon running.
    private(set) var project: URL?

    private static let bookmarkKey = "project-bookmark"

    private func restoreProject() {
        guard let data = UserDefaults.standard.data(forKey: Self.bookmarkKey) else { return }

        var stale = false
        guard let url = try? URL(
            resolvingBookmarkData: data, options: [], relativeTo: nil,
            bookmarkDataIsStale: &stale)
        else { return }

        _ = url.startAccessingSecurityScopedResource()
        project = url

        if stale { remember(url) }
    }

    private func remember(_ url: URL) {
        project = url
        if let data = try? url.bookmarkData(
            options: [], includingResourceValuesForKeys: nil, relativeTo: nil)
        {
            UserDefaults.standard.set(data, forKey: Self.bookmarkKey)
        }
    }

    func connect() async {
        phase = .looking
        if project == nil { restoreProject() }
        do {
            let from = project ?? URL(fileURLWithPath: NSHomeDirectory())
            let discovery = try Discovery.locate(from: from)
            let client = try DaemonClient(discovery: discovery)
            let store = SessionStore(client: client)
            let gateway = GatewayStore(client: client)
            await store.loadSessions()
            phase = .connected(store, gateway)
        } catch {
            phase = .failed("\(error)")
        }
    }

    /// Asks which project, then looks again.
    func chooseProject() async {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.message = "Choose the project whose agent this is — the folder holding .JingClaw."

        guard panel.runModal() == .OK, let chosen = panel.url else { return }
        _ = chosen.startAccessingSecurityScopedResource()
        remember(chosen)
        await connect()
    }
}

struct NotRunningView: View {
    let reason: String
    let project: URL?
    let choose: () async -> Void
    let retry: () async -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("No daemon").font(.title2).bold()

            if let project {
                Text("Looking in \(project.path)").font(.callout)
            } else {
                Text("No project chosen yet.").font(.callout)
            }

            Text(reason)
                .font(.system(.caption, design: .monospaced))
                .textSelection(.enabled)
                .foregroundStyle(.secondary)

            Text("Start one with:")
            Text("agentd").font(.system(.body, design: .monospaced))
                .padding(6)
                .background(.quaternary, in: RoundedRectangle(cornerRadius: 6))

            HStack {
                Button("Choose project…") { Task { await choose() } }
                Button("Look again") { Task { await retry() } }
            }
        }
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}
