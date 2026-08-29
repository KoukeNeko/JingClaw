package viewfixture

import "encoding/json"

// Cases is the agreed set.
//
// Each is a thing a client gets wrong when written from the documentation
// rather than from the log: deltas that have to be joined, a tool call that
// belongs to the turn that asked for it, a fold that makes earlier turns
// something the model no longer has.
func Cases() []Case {
	return []Case{
		{
			Name: "deltas are joined into one message",
			Why: "A client that draws each delta shows one word per line. " +
				"They are pieces of a message, not messages.",
			Events: []Event{
				user(1, "msg_1", "hello"),
				delta(2, "msg_2", "Hel"),
				delta(3, "msg_2", "lo the"),
				delta(4, "msg_2", "re."),
				completed(5, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "hello"},
					{Role: RoleAssistant, Text: "Hello there."},
				},
				HeadSeq: 5,
			},
		},
		{
			Name: "the working-out is kept apart from the answer",
			Why: "It arrives before the turn has any text, so a client that " +
				"joined the two would open the reply with the thinking and " +
				"then forward the whole thing as the answer.",
			Events: []Event{
				user(1, "msg_1", "what time is it in Taipei?"),
				reasoning(2, "msg_2", "They did not say where they are. "),
				reasoning(3, "msg_2", "Taipei is UTC+8, no daylight saving."),
				delta(4, "msg_2", "It is 14:20 in Taipei."),
				completed(5, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "what time is it in Taipei?"},
					{
						Role: RoleAssistant,
						Text: "It is 14:20 in Taipei.",
						Reasoning: "They did not say where they are. " +
							"Taipei is UTC+8, no daylight saving.",
					},
				},
				HeadSeq: 5,
			},
		},
		{
			Name: "a tool call belongs to the turn that asked for it",
			Why: "Drawn as its own entry, a call floats between turns and a " +
				"reader cannot tell which request caused it.",
			Events: []Event{
				user(1, "msg_1", "read it"),
				delta(2, "msg_2", "Looking."),
				toolRequested(3, "call_1", "read_file"),
				toolCompleted(4, "call_1", "read_file", false),
				completed(5, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "read it"},
					{
						Role: RoleAssistant, Text: "Looking.",
						ToolCalls: []ToolCall{{Name: "read_file", Completed: true}},
					},
				},
				HeadSeq: 5,
			},
		},
		{
			Name: "a failed call is not a finished one",
			Why: "A client showing every completed call the same way tells " +
				"somebody the work was done.",
			Events: []Event{
				user(1, "msg_1", "run it"),
				toolRequested(2, "call_1", "exec_command"),
				toolCompleted(3, "call_1", "exec_command", true),
				completed(4, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "run it"},
					{
						Role: RoleAssistant,
						ToolCalls: []ToolCall{
							{Name: "exec_command", Completed: true, IsError: true},
						},
					},
				},
				HeadSeq: 4,
			},
		},
		{
			Name: "an approval waits until it is resolved",
			Why: "A client that forgets a pending approval shows a session " +
				"that looks finished and is blocked on somebody.",
			// An approval only ever happens inside a run, so the case says
			// so: a run parked on a person is still active, and a client
			// that shows otherwise offers no way to interrupt it.
			Events: []Event{
				running(1, "run_1"),
				user(2, "msg_1", "write it"),
				toolRequested(3, "call_1", "write_file"),
				approvalRequested(4, "apr_1"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "write it"},
					{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "write_file"}}},
				},
				PendingApprovals: []string{"apr_1"},
				ActiveRun:        "run_1",
				HeadSeq:          4,
			},
		},
		{
			Name: "a resolved approval stops waiting",
			Why:  "Otherwise it waits forever and the run looks stuck.",
			Events: []Event{
				user(1, "msg_1", "write it"),
				toolRequested(2, "call_1", "write_file"),
				approvalRequested(3, "apr_1"),
				approvalResolved(4, "apr_1"),
				toolCompleted(5, "call_1", "write_file", false),
				completed(6, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "write it"},
					{
						Role:      RoleAssistant,
						ToolCalls: []ToolCall{{Name: "write_file", Completed: true}},
					},
				},
				HeadSeq: 6,
			},
		},
		{
			Name: "a fold replaces what came before it",
			Why: "Drawing turns the model no longer has shows a conversation " +
				"the agent cannot remember, and the next answer looks like " +
				"amnesia rather than compaction.",
			Events: []Event{
				user(1, "msg_1", "first"),
				delta(2, "msg_2", "one"),
				completed(3, "msg_2"),
				compacted(4, 3),
				user(5, "msg_3", "second"),
				delta(6, "msg_4", "two"),
				completed(7, "msg_4"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleAssistant, Text: foldNotice},
					{Role: RoleUser, Text: "second"},
					{Role: RoleAssistant, Text: "two"},
				},
				HeadSeq: 7,
			},
		},
		{
			Name: "a run that is still going is named",
			Why: "A client that does not know a run is active offers no way " +
				"to interrupt it.",
			Events: []Event{
				running(1, "run_1"),
				user(2, "msg_1", "go"),
			},
			Expected: State{
				Messages:  []Message{{Role: RoleUser, Text: "go"}},
				ActiveRun: "run_1",
				HeadSeq:   2,
			},
		},
		{
			Name: "a finished run is not active",
			Why:  "Otherwise a client shows a spinner over a conversation that ended.",
			Events: []Event{
				running(1, "run_1"),
				user(2, "msg_1", "go"),
				delta(3, "msg_2", "done"),
				completed(4, "msg_2"),
				finished(5, "run_1"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "go"},
					{Role: RoleAssistant, Text: "done"},
				},
				HeadSeq: 5,
			},
		},
	}
}

// foldNotice is what a client puts where the folded turns were.
const foldNotice = "[earlier turns were folded into a summary]"

func body(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func user(seq uint64, message, text string) Event {
	return Event{Seq: seq, Kind: "user.message", Body: body(map[string]string{
		"message_id": message, "text": text,
	})}
}

func delta(seq uint64, message, text string) Event {
	return Event{Seq: seq, Kind: "assistant.delta", Body: body(map[string]string{
		"message_id": message, "text": text,
	})}
}

func completed(seq uint64, message string) Event {
	return Event{Seq: seq, Kind: "assistant.completed", Body: body(map[string]string{
		"message_id": message,
	})}
}

func reasoning(seq uint64, message, text string) Event {
	return Event{Seq: seq, Kind: "assistant.reasoning", Body: body(map[string]string{
		"message_id": message, "text": text,
	})}
}

func toolRequested(seq uint64, call, name string) Event {
	return Event{Seq: seq, Kind: "tool.requested", Body: body(map[string]string{
		"call_id": call, "name": name,
	})}
}

func toolCompleted(seq uint64, call, name string, failed bool) Event {
	return Event{Seq: seq, Kind: "tool.completed", Body: body(map[string]any{
		"call_id": call, "name": name, "is_error": failed,
	})}
}

func approvalRequested(seq uint64, id string) Event {
	return Event{Seq: seq, Kind: "approval.requested", Body: body(map[string]string{
		"approval_id": id,
	})}
}

func approvalResolved(seq uint64, id string) Event {
	return Event{Seq: seq, Kind: "approval.resolved", Body: body(map[string]string{
		"approval_id": id,
	})}
}

func compacted(seq, through uint64) Event {
	return Event{Seq: seq, Kind: "conversation.compacted", Body: body(map[string]any{
		"through_seq": through,
	})}
}

func running(seq uint64, run string) Event {
	return Event{Seq: seq, Kind: "run.state_changed", Body: body(map[string]string{
		"run_id": run, "status": "running",
	})}
}

func finished(seq uint64, run string) Event {
	return Event{Seq: seq, Kind: "run.state_changed", Body: body(map[string]string{
		"run_id": run, "status": "completed",
	})}
}
