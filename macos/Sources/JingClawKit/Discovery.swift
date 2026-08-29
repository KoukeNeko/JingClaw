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

    public var description: String {
        switch self {
        case .notRunning(let searched):
            let places = searched.map(\.path).joined(separator: "\n  ")
            return "no daemon found. Looked in:\n  \(places)"
        case .unreadable(let url, let error):
            return "could not read \(url.path): \(error)"
        }
    }
}

extension Discovery {
    /// The name of the file, wherever it is put.
    public static let fileName = "daemon.json"

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

        for candidate in candidates(from: directory, environment: environment) {
            searched.append(candidate)
            guard FileManager.default.isReadableFile(atPath: candidate.path) else { continue }

            do {
                let data = try Data(contentsOf: candidate)
                return try JSONDecoder().decode(Discovery.self, from: data)
            } catch {
                throw DiscoveryError.unreadable(candidate, error)
            }
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
