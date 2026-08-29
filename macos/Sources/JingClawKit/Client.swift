import Foundation

/// Talks to the daemon.
///
/// Connect over JSON rather than a generated gRPC stack: every method is a
/// POST of a JSON body to a path named after it, and a server stream is the
/// same request answered with length-prefixed frames. That is little enough
/// code to own, and it keeps the client free of a code generator in a language
/// that already has one for protobuf but not for this transport.
public actor DaemonClient {
    private let baseURL: URL
    private let token: String
    private let session: URLSession

    public init(discovery: Discovery, session: URLSession = .shared) throws {
        guard let url = URL(string: discovery.baseURL) else {
            throw ClientError.badBaseURL(discovery.baseURL)
        }
        self.baseURL = url
        self.token = discovery.token
        self.session = session
    }

    /// Calls a method and decodes its answer.
    public func call<Response: Decodable>(
        _ service: String,
        _ method: String,
        _ body: [String: Any] = [:],
        as: Response.Type = Response.self
    ) async throws -> Response {
        let (data, response) = try await session.data(for: request(service, method, body))
        try check(response, data)
        return try JSONDecoder().decode(Response.self, from: data)
    }

    /// Calls a method and ignores its answer, for the ones that return nothing
    /// worth reading.
    @discardableResult
    public func send(
        _ service: String, _ method: String, _ body: [String: Any] = [:]
    ) async throws -> Data {
        let (data, response) = try await session.data(for: request(service, method, body))
        try check(response, data)
        return data
    }

    /// Follows a server stream, yielding each frame as it arrives.
    ///
    /// Connect frames a streamed response as a five-byte header — one flag
    /// byte and a big-endian length — followed by that many bytes of JSON. The
    /// flag marks the final frame, which carries the trailers rather than a
    /// message, so it is read and not yielded.
    public func stream(
        _ service: String, _ method: String, _ body: [String: Any] = [:]
    ) -> AsyncThrowingStream<Data, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    var request = try self.request(service, method, body, streaming: true)
                    request.timeoutInterval = .infinity

                    let (bytes, response) = try await session.bytes(for: request)
                    try check(response, Data())

                    var buffer = Data()
                    for try await byte in bytes {
                        buffer.append(byte)

                        while buffer.count >= 5 {
                            let flags = buffer[buffer.startIndex]
                            let length = buffer[
                                buffer.index(buffer.startIndex, offsetBy: 1)..<buffer.index(
                                    buffer.startIndex, offsetBy: 5)
                            ].reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }

                            guard buffer.count >= 5 + Int(length) else { break }

                            let payload = buffer.subdata(in: 5..<(5 + Int(length)))
                            buffer.removeSubrange(0..<(5 + Int(length)))

                            // The end-of-stream frame carries trailers, not a
                            // message. Yielding it would hand the caller
                            // something it cannot decode.
                            if flags & 0x02 == 0 {
                                continuation.yield(payload)
                            }
                        }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    private func request(
        _ service: String, _ method: String, _ body: [String: Any], streaming: Bool = false
    ) throws -> URLRequest {
        let path = "/jingclaw.control.v1.\(service)/\(method)"
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")

        let payload = try JSONSerialization.data(withJSONObject: body)

        if streaming {
            request.setValue("application/connect+json", forHTTPHeaderField: "Content-Type")
            // A streamed request is framed too: the same five-byte header in
            // front of the one message being sent.
            var framed = Data([0])
            var length = UInt32(payload.count).bigEndian
            withUnsafeBytes(of: &length) { framed.append(contentsOf: $0) }
            framed.append(payload)
            request.httpBody = framed
        } else {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = payload
        }

        return request
    }

    private func check(_ response: URLResponse, _ data: Data) throws {
        guard let http = response as? HTTPURLResponse else { return }
        guard http.statusCode == 200 else {
            throw ClientError.refused(status: http.statusCode, body: String(decoding: data, as: UTF8.self))
        }
    }
}

public enum ClientError: Error, CustomStringConvertible {
    case badBaseURL(String)
    case refused(status: Int, body: String)

    public var description: String {
        switch self {
        case .badBaseURL(let url):
            return "the daemon published an unusable address: \(url)"
        case .refused(let status, let body):
            return "the daemon refused the call (\(status)): \(body)"
        }
    }
}
