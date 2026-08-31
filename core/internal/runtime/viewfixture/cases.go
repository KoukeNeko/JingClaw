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
						ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Completed: true}},
					},
				},
				HeadSeq: 5,
			},
		},
		{
			Name: "output too large to show is still reachable",
			Why: "A client that drops the artifact id shows a truncated build " +
				"log with no way to reach the rest, and reopening the session " +
				"loses it entirely.",
			Events: []Event{
				user(1, "msg_1", "run the tests"),
				toolRequested(2, "call_1", "exec_command"),
				toolStored(3, "call_1", "exec_command", "art_1"),
				delta(4, "msg_2", "One test failed."),
				completed(5, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "run the tests"},
					{
						Role: RoleAssistant, Text: "One test failed.",
						ToolCalls: []ToolCall{
							{ID: "call_1", Name: "exec_command", Completed: true, Artifact: "art_1"},
						},
					},
				},
				HeadSeq: 5,
			},
		},
		{
			Name: "the plan is replaced, not accumulated",
			Why: "The event carries the whole plan after each change. A " +
				"client that appended would show every step twice and the " +
				"finished ones as still pending.",
			Events: []Event{
				user(1, "msg_1", "fix the failing test"),
				planned(2, [][3]string{
					{"todo_1", "read the test", "pending"},
					{"todo_2", "fix it", "pending"},
				}),
				planned(3, [][3]string{
					{"todo_1", "read the test", "completed"},
					{"todo_2", "fix it", "in_progress"},
				}),
				delta(4, "msg_2", "Working on it."),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "fix the failing test"},
					{Role: RoleAssistant, Text: "Working on it."},
				},
				Plan: []PlanStep{
					{ID: "todo_1", Title: "read the test", Status: "completed"},
					{ID: "todo_2", Title: "fix it", Status: "in_progress"},
				},
				HeadSeq: 4,
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
							{ID: "call_1", Name: "exec_command", Completed: true, IsError: true},
						},
					},
				},
				HeadSeq: 4,
			},
		},
		{
			Name: "two calls of one tool come back to the right places",
			Why: "Tools run at the same time and need not return in the " +
				"order they were asked for, so a client matching a result " +
				"on the tool's name puts the failure on whichever call it " +
				"reaches first and reports a result against a call that " +
				"did not produce it.",
			Events: []Event{
				user(1, "msg_1", "read both"),
				toolRequested(2, "call_1", "read_file"),
				toolRequested(3, "call_2", "read_file"),
				// The first one back first, which is the whole case. A search
				// by name walks the calls backwards and reaches call_2, so
				// this failure lands on the call that had not answered yet.
				toolCompleted(4, "call_1", "read_file", true),
				toolCompleted(5, "call_2", "read_file", false),
				completed(6, "msg_2"),
			},
			Expected: State{
				Messages: []Message{
					{Role: RoleUser, Text: "read both"},
					{
						Role: RoleAssistant,
						ToolCalls: []ToolCall{
							{ID: "call_1", Name: "read_file", Completed: true, IsError: true},
							{ID: "call_2", Name: "read_file", Completed: true},
						},
					},
				},
				HeadSeq: 6,
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
					{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Name: "write_file"}}},
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
						ToolCalls: []ToolCall{{ID: "call_1", Name: "write_file", Completed: true}},
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
// Written out rather than taken from domain.FoldNotice, and that is the
// point: the reference is only worth having while it can disagree. Sharing
// the constant would make the recorded cases agree with any wording the
// implementation happened to pick, including a wrong one.
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

// planned is one plan announcement. The steps are given as id, title and
// status so a case reads as a plan rather than as a struct literal.
func planned(seq uint64, steps [][3]string) Event {
	items := make([]map[string]string, 0, len(steps))
	for _, step := range steps {
		items = append(items, map[string]string{
			"id": step[0], "title": step[1], "status": step[2],
		})
	}
	return Event{Seq: seq, Kind: "plan.changed", Body: body(map[string]any{"items": items})}
}

func toolStored(seq uint64, call, name, artifact string) Event {
	return Event{Seq: seq, Kind: "tool.completed", Body: body(map[string]any{
		"call_id": call, "name": name, "is_error": false,
		"artifact": map[string]any{"id": artifact, "size": 91234},
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
