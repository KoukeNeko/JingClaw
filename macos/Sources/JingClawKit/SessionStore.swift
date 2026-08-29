import Foundation
import Observation

/// Holds what one session looks like, and keeps it current.
///
/// The view reads this and nothing else. Every change to what is on screen
/// goes through the shared reducer, so the window shows what the console shows
/// and what the daemon says — the three agreeing is checked by the fixtures,
/// and would mean nothing if the app rendered from somewhere else.
@Observable
@MainActor
public final class SessionStore {
    public private(set) var state = SessionState()
    public private(set) var sessions: [SessionSummary] = []
    public private(set) var status: String = ""
    public private(set) var selected: String?

    private let client: DaemonClient
    private var following: Task<Void, Never>?

    public init(client: DaemonClient) {
        self.client = client
    }

    public func loadSessions() async {
        do {
            let listed: SessionListResponse = try await client.call("SessionService", "ListSessions")
            sessions = listed.sessions ?? []
            status = ""
        } catch {
            status = "\(error)"
        }
    }

    public func newSession(title: String) async {
        do {
            let created: CreateSessionResponse = try await client.call(
                "SessionService", "CreateSession", ["title": title])
            await loadSessions()
            await open(created.session.id)
        } catch {
            status = "\(error)"
        }
    }

    /// Opens a session: the assembled state first, then the stream from where
    /// it stops.
    ///
    /// Not a replay. Rebuilding a week-old conversation from every event is
    /// correct and gets slower with each turn.
    public func open(_ id: String) async {
        following?.cancel()
        selected = id
        state = SessionState()

        do {
            let view: SessionViewResponse = try await client.call(
                "SessionService", "GetSessionView",
                ["sessionId": id, "maxMessages": 200])
            state = view.asState
            status = ""
        } catch {
            status = "\(error)"
        }

        follow(id, after: state.headSeq)
    }

    public func send(_ text: String) async {
        guard let id = selected, !text.isEmpty else { return }
        do {
            try await client.send(
                "SessionService", "SendTurn", ["sessionId": id, "text": text])
        } catch {
            status = "\(error)"
        }
    }

    public func decide(_ approvalID: String, allow: Bool) async {
        do {
            try await client.send(
                "SessionService", "DecideApproval",
                [
                    "approvalId": approvalID,
                    "decision": allow ? "APPROVAL_DECISION_ALLOW" : "APPROVAL_DECISION_DENY",
                ])
        } catch {
            status = "\(error)"
        }
    }

    public func interrupt() async {
        guard !state.activeRun.isEmpty else { return }
        do {
            try await client.send(
                "SessionService", "InterruptRun",
                ["runId": state.activeRun, "reason": "stopped from the macOS client"])
        } catch {
            status = "\(error)"
        }
    }

    /// Follows the stream, reconnecting when it drops.
    ///
    /// Resumed from the last sequence applied rather than from the beginning,
    /// so a dropped connection costs a reconnect and not the history again.
    private func follow(_ id: String, after seq: UInt64) {
        following = Task { [weak self] in
            var resumeFrom = seq
            while !Task.isCancelled {
                guard let self else { return }
                do {
                    let frames = await self.client.stream(
                        "SessionService", "SubscribeEvents",
                        ["sessionId": id, "afterSeq": String(resumeFrom)])

                    for try await frame in frames {
                        if Task.isCancelled { return }
                        guard let event = try? JSONDecoder().decode(EventFrame.self, from: frame),
                            let wire = event.event
                        else { continue }

                        if let applied = wire.asLogEvent {
                            self.state = Reducer.reduce(self.state, applied)
                            resumeFrom = applied.seq
                        }
                    }
                } catch {
                    if Task.isCancelled { return }
                    self.status = "reconnecting — \(error)"
                }

                if Task.isCancelled { return }
                try? await Task.sleep(for: .seconds(1.5))
            }
        }
    }
}

public struct SessionSummary: Decodable, Identifiable, Sendable {
    public var id: String
    public var title: String?
}

struct SessionListResponse: Decodable {
    var sessions: [SessionSummary]?
}

struct CreateSessionResponse: Decodable {
    struct Session: Decodable { var id: String }
    var session: Session
}
