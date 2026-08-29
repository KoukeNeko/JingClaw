import Foundation

/// Stored output the daemon kept because it was too large to put in front of a
/// model — a build log, the whole of a file a tool only quoted.
///
/// Fetched rather than carried in the event: the event says one exists and how
/// large it is, and the bytes come only when somebody asks for them. A client
/// that pulled every artifact as it went would download a test suite's entire
/// output to draw a line saying a test failed.
public struct StoredArtifact: Equatable, Sendable {
    public var id: String
    public var size: Int64
    public var mediaType: String

    public init(id: String, size: Int64 = 0, mediaType: String = "") {
        self.id = id
        self.size = size
        self.mediaType = mediaType
    }
}

extension DaemonClient {
    /// Reads an artifact, up to a limit.
    ///
    /// Bounded on purpose. The thing about an artifact is that it did not fit
    /// somewhere else, and a window into the start of a build log is what
    /// somebody glancing at a failure actually wants; the whole of it is what
    /// `save` is for.
    public func readArtifact(_ id: String, limit: Int64 = 64 * 1024) async throws -> String {
        var collected = Data()

        for try await frame in stream("ArtifactService", "ReadArtifact", [
            "meta": ["clientId": DaemonClient.clientName],
            "id": id,
            "limit": limit,
        ]) {
            guard
                let message = try JSONSerialization.jsonObject(with: frame) as? [String: Any],
                let encoded = message["chunk"] as? String,
                // Connect's JSON encoding puts bytes in base64, so a frame is
                // not the bytes themselves.
                let chunk = Data(base64Encoded: encoded)
            else { continue }

            collected.append(chunk)
            if collected.count >= Int(limit) { break }
        }

        return String(decoding: collected, as: UTF8.self)
    }

    /// Writes an artifact to a file, whole.
    public func saveArtifact(_ id: String, to url: URL) async throws {
        FileManager.default.createFile(atPath: url.path, contents: nil)
        let handle = try FileHandle(forWritingTo: url)
        defer { try? handle.close() }

        for try await frame in stream("ArtifactService", "ReadArtifact", [
            "meta": ["clientId": DaemonClient.clientName],
            "id": id,
        ]) {
            guard
                let message = try JSONSerialization.jsonObject(with: frame) as? [String: Any],
                let encoded = message["chunk"] as? String,
                let chunk = Data(base64Encoded: encoded)
            else { continue }

            try handle.write(contentsOf: chunk)
        }
    }
}
