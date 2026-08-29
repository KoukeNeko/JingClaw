import Foundation

/// Where the daemon published itself, and the credential to reach it.
///
/// The daemon binds a loopback port chosen at startup and writes this file, so
/// a client discovers rather than assumes. The file is owner-readable only
/// because it carries the control token.
public struct Discovery: Sendable, Decodable {
    public var pid: Int32
    public var baseURL: String
    public var token: String
    public var protocolVersion: String?

    private enum CodingKeys: String, CodingKey {
        case pid
        case baseURL = "base_url"
        case token
        case protocolVersion = "protocol_version"
    }
}

public enum DiscoveryError: Error, CustomStringConvertible {
    case notRunning(searched: [URL])
    case unreadable(URL, Error)

    /// The directory exists and this process may not look inside it.
    ///
    /// Its own case because it is the opposite problem from a missing daemon
    /// and has the opposite fix. macOS refuses an application access to
    /// Documents, Desktop and Downloads until somebody grants it, and a
    /// client that reports "no daemon" for that sends people to restart one
    /// that was already running.
    case notPermitted(URL)

    public var description: String {
        switch self {
        case .notRunning(let searched):
            let places = searched.map(\.path).joined(separator: "\n  ")
            return "no daemon found. Looked in:\n  \(places)"
        case .unreadable(let url, let error):
            return "could not read \(url.path): \(error)"
        case .notPermitted(let url):
            return """
                not allowed to look inside \(url.path).

                macOS asks before an application reads Documents, Desktop or \
                Downloads. Choose the project folder to grant it.
                """
        }
    }
}

extension Discovery {
    /// The name of the file, wherever it is put.
    public static let fileName = "daemon.json"

    /// Whether the process that wrote this is still there.
    ///
    /// Signal zero asks the kernel about a process without disturbing it. A
    /// permission error means it exists and belongs to somebody else, which
    /// still answers the question being asked.
    public var isRunning: Bool {
        if pid <= 0 { return false }
        if kill(pid, 0) == 0 { return true }
        return errno == EPERM
    }

    /// Finds the running daemon.
    ///
    /// The same order the daemon itself resolves: an explicit directory, a
    /// .JingClaw found by walking up, then the platform's own location. A
    /// client that searched differently would fail to find a daemon that is
    /// running, which is the least helpful way to be wrong.
    public static func locate(
        from directory: URL = URL(fileURLWithPath: FileManager.default.currentDirectoryPath),
        environment: [String: String] = ProcessInfo.processInfo.environment
    ) throws -> Discovery {
        var searched: [URL] = []

        // Asked before the search, because a directory this process may not
        // look inside answers every question the same way a missing one does.
        if let blocked = unreadableProject(directory, environment: environment) {
            throw DiscoveryError.notPermitted(blocked)
        }

        for candidate in candidates(from: directory, environment: environment) {
            searched.append(candidate)
            guard FileManager.default.isReadableFile(atPath: candidate.path) else { continue }

            let found: Discovery
            do {
                let data = try Data(contentsOf: candidate)
                found = try JSONDecoder().decode(Discovery.self, from: data)
            } catch {
                throw DiscoveryError.unreadable(candidate, error)
            }

            // A daemon that stopped without tidying up leaves this file
            // behind, and its port is very likely somebody else's by now.
            // Connecting to it is worse than reporting nothing: the client
            // works, and talks to the wrong thing.
            guard found.isRunning else { continue }
            return found
        }

        throw DiscoveryError.notRunning(searched: searched)
    }

    static func candidates(from directory: URL, environment: [String: String]) -> [URL] {
        var found: [URL] = []

        if let named = environment["JINGCLAW_RUNTIME_DIR"], !named.isEmpty {
            found.append(URL(fileURLWithPath: named).appendingPathComponent(fileName))
        }

        if let home = jingClawDirectory(from: directory, environment: environment) {
            found.append(home.appendingPathComponent("run").appendingPathComponent(fileName))
        }

        // The platform's own location, which is where a daemon with no
        // .JingClaw writes.
        if let support = try? FileManager.default.url(
            for: .applicationSupportDirectory, in: .userDomainMask,
            appropriateFor: nil, create: false
        ) {
            found.append(
                support
                    .appendingPathComponent("JingClaw")
                    .appendingPathComponent("run")
                    .appendingPathComponent(fileName))
        }

        return found
    }

    /// The directory a caller pointed at, when it exists and cannot be read.
    ///
    /// Distinguished from absent by asking the filesystem twice: something is
    /// there, and its contents cannot be listed. That is what a refused
    /// permission looks like from here.
    static func unreadableProject(_ directory: URL, environment: [String: String]) -> URL? {
        guard environment["JINGCLAW_RUNTIME_DIR"] == nil else { return nil }

        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: directory.path, isDirectory: &isDirectory),
            isDirectory.boolValue
        else { return nil }

        if (try? FileManager.default.contentsOfDirectory(atPath: directory.path)) == nil {
            return directory
        }
        return nil
    }

    /// The .JingClaw directory, named outright or found by walking up.
    static func jingClawDirectory(
        from directory: URL, environment: [String: String]
    ) -> URL? {
        if let named = environment["JINGCLAW_HOME"], !named.isEmpty {
            if named == "none" { return nil }
            return URL(fileURLWithPath: named)
        }

        var current = directory.standardizedFileURL
        while true {
            let candidate = current.appendingPathComponent(".JingClaw")
            var isDirectory: ObjCBool = false
            if FileManager.default.fileExists(atPath: candidate.path, isDirectory: &isDirectory),
                isDirectory.boolValue
            {
                return candidate
            }

            let parent = current.deletingLastPathComponent().standardizedFileURL
            if parent == current { return nil }
            current = parent
        }
    }
}
