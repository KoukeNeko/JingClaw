import Foundation

/// The wire shapes a client decodes, and how they become shared state.
///
/// Kept apart from the reducer because they are two different contracts: the
/// reducer is what three clients agree to compute, and this is what one
/// transport happens to send. Mixing them would make the agreed behaviour
/// depend on a JSON field name.

/// GetSessionView's answer.
public struct SessionViewResponse: Decodable, Sendable {
    public var messages: [WireMessage]?
    public var pendingApprovals: [WireApproval]?
    public var activeRun: WireRun?
    public var headSeq: String?
    public var truncated: Bool?

    public struct WireMessage: Decodable, Sendable {
        public var role: String?
        public var text: String?
        public var toolCalls: [WireToolCall]?
    }

    public struct WireToolCall: Decodable, Sendable {
        public var name: String?
        public var summary: String?
        public var completed: Bool?
        public var isError: Bool?
    }

    public struct WireApproval: Decodable, Sendable {
        public var id: String?
        public var toolName: String?
        public var summary: String?
    }

    public struct WireRun: Decodable, Sendable {
        public var id: String?
        public var status: String?
    }

    /// The state a client starts from.
    ///
    /// A sequence arrives as a string because a 64-bit integer does not
    /// survive a JSON number, and an absent one means zero: proto3 omits
    /// default values, so a fresh session has no head sequence at all.
    public var asState: SessionState {
        SessionState(
            messages: (messages ?? []).map { wire in
                Message(
                    role: wire.role == "MESSAGE_ROLE_USER" ? .user : .assistant,
                    text: wire.text ?? "",
                    toolCalls: (wire.toolCalls ?? []).map {
                        ToolCall(
                            name: $0.name ?? "",
                            completed: $0.completed ?? false,
                            isError: $0.isError ?? false)
                    })
            },
            pendingApprovals: (pendingApprovals ?? []).compactMap(\.id),
            activeRun: activeRun?.id ?? "",
            headSeq: UInt64(headSeq ?? "0") ?? 0
        )
    }
}

/// One frame of SubscribeEvents.
public struct EventFrame: Decodable, Sendable {
    public var event: WireEvent?
    public var hello: Hello?
    public var resyncRequired: Resync?

    public struct Hello: Decodable, Sendable {
        public var headSeq: String?
    }

    /// History this client asked for has been discarded.
    ///
    /// Carrying on with whatever survived would draw a conversation missing
    /// its middle, with nothing marking the gap.
    public struct Resync: Decodable, Sendable {
        public var oldestSeq: String?
        public var headSeq: String?
    }
}

/// An event as the wire carries it, and how it becomes one the reducer knows.
///
/// The translation is here rather than in the reducer so that the shared
/// behaviour does not depend on which transport delivered it. A second
/// transport would write another of these and the reducer would not change.
public struct WireEvent: Decodable, Sendable {
    public var seq: String?

    public var userMessageAdded: UserMessage?
    public var assistantTextDelta: TextDelta?
    public var toolCallRequested: ToolCall?
    public var toolCallCompleted: ToolResult?
    public var approvalRequested: Approval?
    public var approvalResolved: Approval?
    public var conversationCompacted: Compacted?
    public var runStateChanged: RunState?

    public struct UserMessage: Decodable, Sendable { public var text: String? }
    public struct TextDelta: Decodable, Sendable { public var text: String? }
    public struct ToolCall: Decodable, Sendable { public var name: String? }
    public struct ToolResult: Decodable, Sendable {
        public var name: String?
        public var isError: Bool?
    }
    public struct Approval: Decodable, Sendable { public var approvalId: String? }
    public struct Compacted: Decodable, Sendable { public var throughSeq: String? }
    public struct RunState: Decodable, Sendable { public var status: String? }

    /// The event in the shape every client's reducer takes, or nil for the
    /// kinds a screen does not show.
    public var asLogEvent: LogEvent? {
        let sequence = UInt64(seq ?? "0") ?? 0

        if let payload = userMessageAdded {
            return LogEvent(seq: sequence, kind: "user.message",
                body: EventBody(text: payload.text))
        }
        if let payload = assistantTextDelta {
            return LogEvent(seq: sequence, kind: "assistant.delta",
                body: EventBody(text: payload.text))
        }
        if let payload = toolCallRequested {
            return LogEvent(seq: sequence, kind: "tool.requested",
                body: EventBody(name: payload.name))
        }
        if let payload = toolCallCompleted {
            return LogEvent(seq: sequence, kind: "tool.completed",
                body: EventBody(name: payload.name, isError: payload.isError))
        }
        if let payload = approvalRequested {
            return LogEvent(seq: sequence, kind: "approval.requested",
                body: EventBody(approvalID: payload.approvalId))
        }
        if let payload = approvalResolved {
            return LogEvent(seq: sequence, kind: "approval.resolved",
                body: EventBody(approvalID: payload.approvalId))
        }
        if conversationCompacted != nil {
            return LogEvent(seq: sequence, kind: "conversation.compacted", body: EventBody())
        }
        if let payload = runStateChanged {
            // The wire spells a status RUN_STATUS_RUNNING; the shared cases
            // spell it running, because they are about behaviour rather than
            // about one transport's enum names.
            let status = (payload.status ?? "")
                .replacingOccurrences(of: "RUN_STATUS_", with: "")
                .lowercased()
            return LogEvent(seq: sequence, kind: "run.state_changed",
                body: EventBody(runID: "run", status: status))
        }

        return nil
    }
}
