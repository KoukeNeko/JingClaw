import Foundation

/// What a client shows for one session.
///
/// Deliberately small: the parts three clients must agree on, not everything
/// any one of them displays. A property here is a promise that Swift,
/// TypeScript and Go all compute it the same way, and
/// `fixtures/session-view.json` is where that promise is checked.
public struct SessionState: Equatable, Sendable {
    public var messages: [Message]

    /// Approvals still waiting on somebody, in the order they were raised.
    public var pendingApprovals: [String]

    /// The run in flight, empty when none is.
    public var activeRun: String

    /// The last event accounted for, which is where a client resumes.
    public var headSeq: UInt64

    public init(
        messages: [Message] = [],
        pendingApprovals: [String] = [],
        activeRun: String = "",
        headSeq: UInt64 = 0
    ) {
        self.messages = messages
        self.pendingApprovals = pendingApprovals
        self.activeRun = activeRun
        self.headSeq = headSeq
    }
}

/// One turn as a client draws it.
public struct Message: Equatable, Sendable {
    public enum Role: String, Sendable {
        case user
        case assistant
    }

    public var role: Role
    public var text: String
    public var toolCalls: [ToolCall]

    public init(role: Role, text: String = "", toolCalls: [ToolCall] = []) {
        self.role = role
        self.text = text
        self.toolCalls = toolCalls
    }
}

/// One tool a turn asked for.
public struct ToolCall: Equatable, Sendable {
    public var name: String
    public var completed: Bool
    public var isError: Bool

    public init(name: String, completed: Bool = false, isError: Bool = false) {
        self.name = name
        self.completed = completed
        self.isError = isError
    }
}

/// One entry from the log, in the shape a client receives it.
public struct LogEvent: Sendable, Decodable {
    public var seq: UInt64
    public var kind: String
    public var body: EventBody

    public init(seq: UInt64, kind: String, body: EventBody = EventBody()) {
        self.seq = seq
        self.kind = kind
        self.body = body
    }
}

/// The fields the shared reducer reads.
///
/// Typed rather than a dictionary. The set of fields that decide what a client
/// shows is small and fixed, and writing it down is what makes the contract
/// between three languages something a compiler can check rather than
/// something each of them looks up by string.
public struct EventBody: Sendable, Decodable {
    public var text: String?
    public var name: String?
    public var isError: Bool?
    public var approvalID: String?
    public var runID: String?
    public var status: String?

    public init(
        text: String? = nil,
        name: String? = nil,
        isError: Bool? = nil,
        approvalID: String? = nil,
        runID: String? = nil,
        status: String? = nil
    ) {
        self.text = text
        self.name = name
        self.isError = isError
        self.approvalID = approvalID
        self.runID = runID
        self.status = status
    }

    private enum CodingKeys: String, CodingKey {
        case text
        case name
        case isError = "is_error"
        case approvalID = "approval_id"
        case runID = "run_id"
        case status
    }
}
