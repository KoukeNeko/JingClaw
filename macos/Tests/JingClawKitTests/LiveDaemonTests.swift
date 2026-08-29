import Foundation
import Testing

@testable import JingClawKit

/// These talk to a daemon that is actually running, and skip when none is.
///
/// Everything else here is checked against fixtures written by hand. A client
/// that parses fixtures perfectly and cannot open a stream is a client that
/// does not work, and that difference has cost this project real defects
/// before.
private func liveClient() throws -> DaemonClient? {
    let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().deletingLastPathComponent()

    guard let discovery = try? Discovery.locate(from: root) else { return nil }
    return try DaemonClient(discovery: discovery)
}

private struct SessionList: Decodable {
    struct Session: Decodable {
        var id: String
        var title: String?
    }
    var sessions: [Session]?
}

private struct CreatedSession: Decodable {
    struct Session: Decodable { var id: String }
    var session: Session
}

private struct ViewResponse: Decodable {
    var headSeq: String?
    var messages: [ViewMessage]?

    struct ViewMessage: Decodable {
        var role: String?
        var text: String?
    }
}

@Test("the client finds and reads a running daemon")
func readsARunningDaemon() async throws {
    guard let client = try liveClient() else {
        // No daemon: not a failure, and saying so beats a green run that
        // checked nothing.
        print("skipped: no daemon is running")
        return
    }

    let listed: SessionList = try await client.call("SessionService", "ListSessions")
    #expect(listed.sessions != nil, "the daemon answered without a session list")
}

@Test("the client can start a session and read it back")
func startsAndReadsASession() async throws {
    guard let client = try liveClient() else {
        print("skipped: no daemon is running")
        return
    }

    let created: CreatedSession = try await client.call(
        "SessionService", "CreateSession", ["title": "from the macOS client"])
    #expect(!created.session.id.isEmpty)

    let view: ViewResponse = try await client.call(
        "SessionService", "GetSessionView",
        ["sessionId": created.session.id, "maxMessages": 50])

    // A fresh session has nothing in it, and its head is zero — which the
    // wire format omits, because proto3 JSON leaves out default values. A
    // client that reads an absent field as an error refuses to open every
    // new session; absent means zero and has to be read that way.
    let resumeFrom = UInt64(view.headSeq ?? "0") ?? 0
    #expect(resumeFrom == 0, "a fresh session resumes from \(resumeFrom)")
    #expect((view.messages ?? []).isEmpty, "a fresh session already has messages")
}

private struct EventFrame: Decodable {
    struct Hello: Decodable { var headSeq: String? }
    struct Event: Decodable {
        var seq: String?
        var userMessageAdded: UserMessage?
        struct UserMessage: Decodable { var text: String? }
    }

    var hello: Hello?
    var event: Event?
    var heartbeat: Heartbeat?
    struct Heartbeat: Decodable {}
}

@Test("the client can follow a session's stream")
func followsAStream() async throws {
    guard let client = try liveClient() else {
        print("skipped: no daemon is running")
        return
    }

    let created: CreatedSession = try await client.call(
        "SessionService", "CreateSession", ["title": "streaming from macOS"])

    // Something to receive. The fake echo is not needed: the user's own turn
    // is recorded before any model is asked, so it arrives regardless of what
    // the provider does.
    try await client.send(
        "SessionService", "SendTurn",
        ["sessionId": created.session.id, "text": "hello from the macOS client"])

    let stream = await client.stream(
        "SessionService", "SubscribeEvents",
        ["sessionId": created.session.id, "afterSeq": "0"])

    var sawHello = false
    var sawOurText = false

    // Bounded, because a stream that never ends is how a test hangs a suite.
    let deadline = Date().addingTimeInterval(20)
    for try await frame in stream {
        let decoded = try JSONDecoder().decode(EventFrame.self, from: frame)
        if decoded.hello != nil { sawHello = true }
        if decoded.event?.userMessageAdded?.text == "hello from the macOS client" {
            sawOurText = true
            break
        }
        if Date() > deadline { break }
    }

    #expect(sawHello, "the stream never opened with a hello frame")
    #expect(sawOurText, "the turn that was sent never arrived on the stream")
}

@Test("a discovery file whose daemon has gone is not used")
func refusesAStaleDiscoveryFile() throws {
    let directory = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent(UUID().uuidString)
    let runtime = directory.appendingPathComponent("run")
    try FileManager.default.createDirectory(at: runtime, withIntermediateDirectories: true)

    // A pid that cannot be running: the daemon stopped without tidying up,
    // and its port is very likely somebody else's by now. Connecting to it is
    // worse than finding nothing, because the client works and talks to the
    // wrong thing.
    let stale = """
        {"pid": 2147483000, "base_url": "http://127.0.0.1:1", "token": "x"}
        """
    try stale.write(
        to: runtime.appendingPathComponent(Discovery.fileName),
        atomically: true, encoding: .utf8)

    #expect(throws: (any Error).self) {
        _ = try Discovery.locate(
            from: directory,
            environment: ["JINGCLAW_RUNTIME_DIR": runtime.path])
    }
}

@Test("a discovery file whose daemon is running is used")
func acceptsALiveDiscoveryFile() throws {
    let directory = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent(UUID().uuidString)
    let runtime = directory.appendingPathComponent("run")
    try FileManager.default.createDirectory(at: runtime, withIntermediateDirectories: true)

    // This process is certainly running, which is the point.
    let live = """
        {"pid": \(ProcessInfo.processInfo.processIdentifier), \
        "base_url": "http://127.0.0.1:1", "token": "x"}
        """
    try live.write(
        to: runtime.appendingPathComponent(Discovery.fileName),
        atomically: true, encoding: .utf8)

    let found = try Discovery.locate(
        from: directory, environment: ["JINGCLAW_RUNTIME_DIR": runtime.path])
    #expect(found.token == "x")
}

@Test("a folder that cannot be read is not reported as no daemon")
func tellsRefusedPermissionFromNoDaemon() throws {
    let directory = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)

    // Unreadable to anybody, which is what a refused permission looks like
    // from inside the process.
    try FileManager.default.setAttributes(
        [.posixPermissions: 0o000], ofItemAtPath: directory.path)
    defer {
        try? FileManager.default.setAttributes(
            [.posixPermissions: 0o755], ofItemAtPath: directory.path)
    }

    do {
        _ = try Discovery.locate(from: directory, environment: [:])
        Issue.record("an unreadable folder was accepted")
    } catch let error as DiscoveryError {
        guard case .notPermitted = error else {
            Issue.record("""
                reported \(error) for a folder it may not read.
                Telling somebody no daemon is running sends them to restart one \
                that already is.
                """)
            return
        }
    }
}
