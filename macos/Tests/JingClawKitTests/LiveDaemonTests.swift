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
