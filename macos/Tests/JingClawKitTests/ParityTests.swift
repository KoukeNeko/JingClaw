import Foundation
import Testing

@testable import JingClawKit

/// The cases every client must agree on.
///
/// Read from the repository's own fixtures rather than copied into the test
/// bundle. A copy is a thing that goes stale quietly, and the whole purpose of
/// these is that three implementations are checked against one file.
private struct FixtureFile: Decodable {
    var cases: [FixtureCase]
}

private struct FixtureCase: Decodable {
    var name: String
    var why: String
    var events: [LogEvent]
    var expected: ExpectedState
}

private struct ExpectedState: Decodable {
    var messages: [ExpectedMessage]?
    var pendingApprovals: [String]?
    var activeRun: String?
    var headSeq: UInt64?

    private enum CodingKeys: String, CodingKey {
        case messages
        case pendingApprovals = "pending_approvals"
        case activeRun = "active_run"
        case headSeq = "head_seq"
    }

    var state: SessionState {
        SessionState(
            messages: (messages ?? []).map(\.message),
            pendingApprovals: pendingApprovals ?? [],
            activeRun: activeRun ?? "",
            headSeq: headSeq ?? 0
        )
    }
}

private struct ExpectedMessage: Decodable {
    var role: String
    var text: String?
    var toolCalls: [ExpectedToolCall]?

    private enum CodingKeys: String, CodingKey {
        case role
        case text
        case toolCalls = "tool_calls"
    }

    var message: Message {
        Message(
            role: role == "user" ? .user : .assistant,
            text: text ?? "",
            toolCalls: (toolCalls ?? []).map(\.call)
        )
    }
}

private struct ExpectedToolCall: Decodable {
    var name: String
    var completed: Bool?
    var isError: Bool?

    private enum CodingKeys: String, CodingKey {
        case name
        case completed
        case isError = "is_error"
    }

    var call: ToolCall {
        ToolCall(name: name, completed: completed ?? false, isError: isError ?? false)
    }
}

private func loadFixtures() throws -> [FixtureCase] {
    // Up from Tests/JingClawKitTests to the repository root.
    let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
    let path = root.appendingPathComponent("fixtures/session-view.json")

    let data = try Data(contentsOf: path)
    return try JSONDecoder().decode(FixtureFile.self, from: data).cases
}

@Test("the macOS client agrees with every shared case")
func agreesWithEveryCase() throws {
    let cases = try loadFixtures()

    // A test that silently checks nothing is worse than no test: if the
    // fixtures move, this must fail rather than pass over an empty list.
    #expect(cases.count >= 5, "loaded \(cases.count) cases, which is too few to be the real file")

    for fixture in cases {
        let got = Reducer.reduceAll(fixture.events)
        #expect(
            got == fixture.expected.state,
            """
            \(fixture.name)
            \(fixture.why)
            got:  \(got)
            want: \(fixture.expected.state)
            """
        )
    }
}
