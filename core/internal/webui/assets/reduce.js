// The reducer three clients share.
//
// Turning an event log into a screen is done once per client, in a different
// language each time, and nothing stops them drifting except a set of examples
// they are all checked against. Those live in fixtures/session-view.json; this
// is the implementation they check.
//
// Pure on purpose. The console's rendering used to fold events straight into
// the DOM, which meant the behaviour could not be tested without a browser and
// could not be compared with Swift's at all.

export const ROLE_USER = 'user';
export const ROLE_ASSISTANT = 'assistant';

export const FOLD_NOTICE = '[earlier turns were folded into a summary]';

export function emptyState() {
  return { messages: [], pending_approvals: [], active_run: '', head_seq: 0 };
}

// reduce applies one event and returns the new state.
export function reduce(state, event) {
  const next = {
    messages: state.messages.map(cloneMessage),
    pending_approvals: [...state.pending_approvals],
    active_run: state.active_run,
    head_seq: Number(event.seq),
  };

  const body = event.body || {};

  switch (event.kind) {
    case 'user.message':
      next.messages.push({ role: ROLE_USER, text: body.text || '' });
      break;

    case 'assistant.delta': {
      // Joined onto the open assistant turn. A delta that starts a new message
      // is one word on a line of its own.
      const index = openAssistant(next.messages);
      if (index < 0) next.messages.push({ role: ROLE_ASSISTANT, text: '' });
      const at = index < 0 ? next.messages.length - 1 : index;
      next.messages[at].text += body.text || '';
      break;
    }

    case 'tool.requested': {
      // Attached to the turn that asked, which is the assistant turn being
      // written — creating one if the model asked before saying anything.
      const index = openAssistant(next.messages);
      if (index < 0) next.messages.push({ role: ROLE_ASSISTANT, text: '' });
      const at = index < 0 ? next.messages.length - 1 : index;
      const message = next.messages[at];
      message.tool_calls = message.tool_calls || [];
      message.tool_calls.push({
        name: body.name || '', completed: false, is_error: false,
      });
      break;
    }

    case 'tool.completed':
      markCompleted(next.messages, body.name || '', Boolean(body.is_error));
      break;

    case 'approval.requested':
      next.pending_approvals.push(body.approval_id || '');
      break;

    case 'approval.resolved':
      next.pending_approvals =
        next.pending_approvals.filter((id) => id !== body.approval_id);
      break;

    case 'conversation.compacted':
      // Everything before the fold is a summary now, so what a client draws is
      // the notice and whatever follows.
      next.messages = [{ role: ROLE_ASSISTANT, text: FOLD_NOTICE }];
      break;

    case 'run.state_changed':
      if (['completed', 'failed', 'cancelled'].includes(body.status)) {
        if (next.active_run === body.run_id) next.active_run = '';
      } else {
        next.active_run = body.run_id || '';
      }
      break;

    default:
      break;
  }

  return next;
}

export function reduceAll(events) {
  return events.reduce(reduce, emptyState());
}

function cloneMessage(message) {
  const copy = { role: message.role, text: message.text };
  if (message.tool_calls) copy.tool_calls = message.tool_calls.map((c) => ({ ...c }));
  return copy;
}

// openAssistant is the assistant turn being written, or -1.
//
// The last message when it is the assistant's. A user turn closes it: what
// follows belongs to the answer to that turn, not to the one before it.
function openAssistant(messages) {
  if (messages.length === 0) return -1;
  const last = messages.length - 1;
  return messages[last].role === ROLE_ASSISTANT ? last : -1;
}

// markCompleted finishes the last call of that name still running.
//
// The last rather than the first: names repeat within a turn, and marking the
// first reports the wrong one as done.
function markCompleted(messages, name, failed) {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const calls = messages[i].tool_calls || [];
    for (let j = calls.length - 1; j >= 0; j -= 1) {
      if (calls[j].name === name && !calls[j].completed) {
        calls[j].completed = true;
        calls[j].is_error = failed;
        return;
      }
    }
  }
}
