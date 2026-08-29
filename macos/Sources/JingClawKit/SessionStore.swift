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

    /// What the agent has asked and nobody has answered.
    ///
    /// Its own list rather than folded in with approvals: an approval is
    /// allowed or denied and this is answered, and a panel offering both sets
    /// of controls would offer the wrong one half the time.
    public private(set) var asked: [PendingQuestion] = []

    /// What each waiting approval is actually asking for.
    ///
    /// The reducer keeps the ids, because that is what three clients have to
    /// agree about; the rest is fetched, because approving a call whose
    /// contents you cannot see is not a decision.
    public private(set) var waiting: [PendingApproval] = []

    /// What the provider offers, and which one this session uses.
    ///
    /// Per session rather than per daemon because that is what a local
    /// deployment needs: the small model that fits in memory for most of what
    /// gets asked, and the large one for the conversation that needs it.
    public private(set) var models: [String] = []
    public private(set) var currentModel = ""
    public private(set) var defaultModel = ""

    /// Stored output somebody asked to look at, by artifact id.
    ///
    /// Kept here rather than fetched by the view so that opening the same one
    /// twice does not download it twice, and so a window redraw does not start
    /// a request.
    public private(set) var opened: [String: String] = [:]

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

        await loadApprovals()
        await loadQuestions()
        await loadModels()
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
            // Removed here rather than waiting for the event, so the button
            // somebody just pressed stops being pressable.
            waiting.removeAll { $0.id == approvalID }
        } catch {
            status = "\(error)"
        }
    }

    /// Fetches what the agent is waiting to be told.
    ///
    /// From the daemon rather than accumulated from events, for the same
    /// reason approvals are: a session opened while the agent was waiting
    /// would otherwise look like one that had simply stopped.
    public func loadQuestions() async {
        guard let id = selected else { return }
        do {
            let listed: QuestionListResponse = try await client.call(
                "SessionService", "ListQuestions", ["sessionId": id])
            asked = listed.questions ?? []
        } catch {
            status = "could not read what is being asked: \(error)"
        }
    }

    /// Answers a question, which resumes the run that asked it.
    public func answer(_ questionID: String, with answer: String) async {
        do {
            try await client.send(
                "SessionService", "AnswerQuestion",
                ["questionId": questionID, "answer": answer])
            // Removed here rather than waiting for the event, so the control
            // somebody just used stops being usable.
            asked.removeAll { $0.id == questionID }
        } catch {
            status = "could not answer: \(error)"
        }
    }

    /// Fetches what the provider offers and which one this session uses.
    ///
    /// A daemon that cannot answer leaves the picker empty rather than
    /// stopping the session opening: a client that refused to show a
    /// conversation because a model list was unavailable would be worse than
    /// one without a picker.
    public func loadModels() async {
        guard let id = selected else { return }
        do {
            let answer: ModelListResponse = try await client.call(
                "SessionService", "ListModels", ["sessionId": id])
            models = (answer.models ?? []).map(\.id)
            currentModel = answer.current ?? ""
            defaultModel = answer.defaultModel ?? ""
        } catch {
            models = []
            currentModel = ""
            defaultModel = ""
        }
    }

    /// Chooses which model answers here. The next run picks it up.
    public func chooseModel(_ model: String) async {
        guard let id = selected else { return }
        do {
            try await client.send(
                "SessionService", "SetSessionModel", ["sessionId": id, "model": model])
            currentModel = model.isEmpty ? defaultModel : model
            status = "this session now answers with \(currentModel)"
        } catch {
            status = "could not change the model: \(error)"
            await loadModels()
        }
    }

    /// Fetches what each waiting approval is asking for.
    ///
    /// Called on open and whenever the reducer sees a new one, rather than
    /// read off the event: a session opened rather than watched has approvals
    /// raised before this client was looking, and they need reviewing too.
    public func loadApprovals() async {
        guard let id = selected else { return }
        do {
            let listed: ApprovalListResponse = try await client.call(
                "SessionService", "ListApprovals", ["sessionId": id])
            waiting = listed.approvals ?? []
        } catch {
            status = "could not read what is waiting: \(error)"
        }
    }

    /// Fetches stored output so it can be shown where the reader already is.
    ///
    /// A window into the start of it rather than the whole: what a person
    /// glancing at a failed build wants is the first screenful, and the whole
    /// of a test suite's output is what `save` is for.
    public func openArtifact(_ id: String) async {
        if opened[id] != nil { return }
        do {
            opened[id] = try await client.readArtifact(id)
            status = ""
        } catch {
            status = "could not read \(id): \(error)"
        }
    }

    /// Closes an opened artifact, so the panel folds away and the bytes go.
    public func closeArtifact(_ id: String) {
        opened[id] = nil
    }

    /// Writes an artifact to a file, whole.
    public func saveArtifact(_ id: String, to url: URL) async {
        do {
            try await client.saveArtifact(id, to: url)
            status = "saved \(id)"
        } catch {
            status = "could not save \(id): \(error)"
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
                        guard let decoded = try? JSONDecoder().decode(EventFrame.self, from: frame)
                        else { continue }

                        // The history this client was resuming from is gone.
                        // Reopening from a view is the only way back to a
                        // conversation without a hole in it.
                        if decoded.resyncRequired != nil {
                            self.status = "history was trimmed — reopening"
                            await self.open(id)
                            return
                        }

                        if let applied = decoded.event?.asLogEvent {
                            let before = self.state.pendingApprovals
                            self.state = Reducer.reduce(self.state, applied)
                            resumeFrom = applied.seq

                            if self.state.pendingApprovals != before {
                                await self.loadApprovals()
                            }
                            // A question is not in the shared state, so the
                            // event kind is what says to go and look.
                            if applied.kind == "question.asked"
                                || applied.kind == "question.answered" {
                                await self.loadQuestions()
                            }
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

struct QuestionListResponse: Decodable {
    var questions: [PendingQuestion]?
}

struct PlanResponse: Decodable {
    var plan: [PlanStep]?
}

struct ModelListResponse: Decodable {
    struct Model: Decodable { var id: String }

    var models: [Model]?
    var current: String?

    /// `default` is a Swift keyword, so the wire name is spelled out here
    /// rather than left to the compiler to reject.
    var defaultModel: String?

    private enum CodingKeys: String, CodingKey {
        case models
        case current
        case defaultModel = "default"
    }
}

struct ApprovalListResponse: Decodable {
    var approvals: [PendingApproval]?
}

struct CreateSessionResponse: Decodable {
    struct Session: Decodable { var id: String }
    var session: Session
}
