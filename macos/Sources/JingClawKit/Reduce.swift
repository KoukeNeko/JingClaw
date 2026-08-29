import Foundation

/// What a client puts where folded turns were.
public let foldNotice = "[earlier turns were folded into a summary]"

/// Folds an event log into what a client shows.
///
/// The same reducer as the console's and the daemon's, in a third language,
/// and checked against the same cases. Nothing else stops three
/// implementations drifting apart, and a drift here means somebody watching
/// the same session in two places is told two different things about what the
/// agent did.
public enum Reducer {
    /// Applies one event.
    public static func reduce(_ state: SessionState, _ event: LogEvent) -> SessionState {
        var next = state
        next.headSeq = event.seq

        switch event.kind {
        case "user.message":
            next.messages.append(Message(role: .user, text: event.body.text ?? ""))

        case "assistant.delta":
            // Joined onto the open assistant turn. A delta that starts a new
            // message is one word on a line of its own.
            let index = openAssistant(next.messages) ?? {
                next.messages.append(Message(role: .assistant))
                return next.messages.count - 1
            }()
            next.messages[index].text += event.body.text ?? ""

        case "tool.requested":
            // Attached to the turn that asked, which is the assistant turn
            // being written — creating one if the model asked for a tool
            // before saying anything.
            let index = openAssistant(next.messages) ?? {
                next.messages.append(Message(role: .assistant))
                return next.messages.count - 1
            }()
            next.messages[index].toolCalls.append(ToolCall(name: event.body.name ?? ""))

        case "tool.completed":
            markCompleted(
                &next.messages,
                name: event.body.name ?? "",
                failed: event.body.isError ?? false
            )

        case "approval.requested":
            next.pendingApprovals.append(event.body.approvalID ?? "")

        case "approval.resolved":
            let resolved = event.body.approvalID ?? ""
            next.pendingApprovals.removeAll { $0 == resolved }

        case "conversation.compacted":
            // Everything before the fold is a summary now, so what a client
            // draws is the notice and whatever follows.
            next.messages = [Message(role: .assistant, text: foldNotice)]

        case "run.state_changed":
            let run = event.body.runID ?? ""
            let status = event.body.status ?? ""
            if ["completed", "failed", "cancelled"].contains(status) {
                if next.activeRun == run { next.activeRun = "" }
            } else {
                next.activeRun = run
            }

        default:
            break
        }

        return next
    }

    /// Folds a whole sequence, which is what a client does on attach.
    public static func reduceAll(_ events: [LogEvent]) -> SessionState {
        events.reduce(SessionState()) { reduce($0, $1) }
    }

    /// The assistant turn being written, or nil.
    ///
    /// The last message when it is the assistant's. A user turn closes it:
    /// what follows belongs to the answer to that turn, not to the one before.
    private static func openAssistant(_ messages: [Message]) -> Int? {
        guard let last = messages.indices.last, messages[last].role == .assistant else {
            return nil
        }
        return last
    }

    /// Finishes the last call of that name still running.
    ///
    /// The last rather than the first: names repeat within a turn, and marking
    /// the first reports the wrong one as done.
    private static func markCompleted(_ messages: inout [Message], name: String, failed: Bool) {
        for messageIndex in messages.indices.reversed() {
            for callIndex in messages[messageIndex].toolCalls.indices.reversed() {
                let call = messages[messageIndex].toolCalls[callIndex]
                if call.name == name && !call.completed {
                    messages[messageIndex].toolCalls[callIndex].completed = true
                    messages[messageIndex].toolCalls[callIndex].isError = failed
                    return
                }
            }
        }
    }
}
