// The console talks to the daemon over Connect with JSON bodies, which is why
// there is no bundler and no generated client here: a unary call is a POST
// whose body is the message, and a stream is the same POST answered with
// length-prefixed frames. A build step that has to have run on the machine
// that produced the binary would defeat the point of the binary.
'use strict';

const API = '/jingclaw.control.v1.';
const TOKEN_KEY = 'jingclaw.token';
const REDEEM_PATH = '/pair';
const CLIENT_ID = 'jingclaw-web';
const SESSION_POLL_MS = 4000;
// Enough of a build log to find the failure in, without putting four hundred
// megabytes into the DOM because somebody clicked once.
const ARTIFACT_VIEW_BYTES = 512 * 1024;

const el = (id) => document.getElementById(id);

const state = {
  token: null,
  sessions: [],
  sessionId: null,
  // Highest sequence rendered, so a reconnect resumes rather than repeating.
  seq: 0,
  stream: null,
  approvals: new Map(),
  runId: null,
  runStatus: '',
  // Assistant text arrives as many deltas; they are folded into the line
  // already on screen rather than each becoming one.
  openMessage: null,
};

// ---------------------------------------------------------------- transport

class RPCError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

async function call(service, method, message) {
  const response = await fetch(`${API}${service}/${method}`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${state.token}`,
    },
    body: JSON.stringify(withMeta(message)),
  });

  if (!response.ok) {
    let detail = await response.text();
    let code = String(response.status);
    try {
      const parsed = JSON.parse(detail);
      code = parsed.code || code;
      detail = parsed.message || detail;
    } catch {
      // A plain-text error from the middleware rather than a Connect one.
    }
    throw new RPCError(code, detail);
  }

  return response.json();
}

function withMeta(message) {
  return { meta: { clientId: CLIENT_ID }, ...message };
}

// openStream reads a Connect server stream.
//
// Each frame is a 5-byte prefix — one flag byte, then a big-endian length —
// followed by that many bytes of JSON. The flag marks the final frame, which
// carries the error if there was one rather than the status code doing it.
async function openStream(service, method, message, onMessage, signal) {
  const response = await fetch(`${API}${service}/${method}`, {
    method: 'POST',
    headers: {
      'content-type': 'application/connect+json',
      'connect-protocol-version': '1',
      authorization: `Bearer ${state.token}`,
    },
    // A streaming call's request is enveloped too, not raw JSON: the same
    // five-byte prefix the answer uses. Sending the bare body makes the server
    // read the first four characters as a length and wait for a gigabyte.
    body: envelope(withMeta(message)),
    signal,
  });

  if (!response.ok || !response.body) {
    throw new RPCError(String(response.status), await response.text());
  }

  const reader = response.body.getReader();
  let buffer = new Uint8Array(0);

  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;

    buffer = concat(buffer, value);

    // More than one frame can arrive in a single chunk, and one frame can
    // span several, so the loop drains whatever is complete and keeps the rest.
    for (;;) {
      if (buffer.length < 5) break;

      const view = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);
      const flags = view.getUint8(0);
      const length = view.getUint32(1);
      if (buffer.length < 5 + length) break;

      const payload = buffer.subarray(5, 5 + length);
      buffer = buffer.subarray(5 + length);

      const frame = JSON.parse(new TextDecoder().decode(payload));
      if (flags & 0x02) {
        if (frame.error) throw new RPCError(frame.error.code, frame.error.message);
        return;
      }
      onMessage(frame);
    }
  }
}

// envelope wraps a message the way the Connect streaming protocol expects:
// one flag byte, a big-endian length, then the payload.
function envelope(message) {
  const payload = new TextEncoder().encode(JSON.stringify(message));
  const framed = new Uint8Array(5 + payload.length);

  new DataView(framed.buffer).setUint32(1, payload.length);
  framed.set(payload, 5);

  return framed;
}

function concat(a, b) {
  const joined = new Uint8Array(a.length + b.length);
  joined.set(a);
  joined.set(b, a.length);
  return joined;
}

// ------------------------------------------------------------------ unlock

function readCodeFromURL() {
  const params = new URLSearchParams(location.search);
  const code = params.get('c');
  if (!code) return null;

  // Taken out of the address bar before anything else happens. A code in a URL
  // ends up in history, in a screenshot, and in whatever the next page is told
  // referred it — and it is still good until it is used.
  history.replaceState(null, '', location.pathname);
  return code;
}

// redeem exchanges the code for this browser's own credential.
//
// The code travels through places people can read — a terminal's scrollback,
// an SSH session somebody else can scroll back through — so it works once and
// expires. What it buys is narrower than the credential the CLI holds and can
// be revoked without touching it.
async function redeem(code) {
  const response = await fetch(REDEEM_PATH, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  if (!response.ok) {
    throw new Error('That code is not valid. Run "agent console" for another.');
  }

  const { token } = await response.json();
  if (!token) throw new Error('The daemon returned no credential.');
  return token;
}

async function useToken(token) {
  state.token = token;
  // The cheapest call that proves the credential works, so a dead one says so
  // here rather than by everything quietly failing later.
  await call('SessionService', 'ListSessions', {});

  // localStorage rather than sessionStorage: a code works once, so a second
  // tab has no way to get its own. Sharing what the first tab redeemed is what
  // makes opening a second tab, or reloading, work at all.
  //
  // The daemon usually takes a fresh port each start, which makes each run its
  // own origin and leaves nothing behind that still works. When the port is
  // pinned, a stale credential fails the call above and is cleared.
  localStorage.setItem(TOKEN_KEY, token);
}

async function start() {
  const code = readCodeFromURL();

  if (code) {
    try {
      await useToken(await redeem(code));
    } catch (err) {
      localStorage.removeItem(TOKEN_KEY);
      return showLocked(err.message);
    }
  } else {
    const stored = localStorage.getItem(TOKEN_KEY);
    if (!stored) return showLocked();

    try {
      await useToken(stored);
    } catch {
      localStorage.removeItem(TOKEN_KEY);
      return showLocked('This browser is no longer paired with the daemon.');
    }
  }

  el('locked').hidden = true;
  el('app').hidden = false;
  await refreshSessions();

  // Sessions are started from other places — a terminal, a Discord message —
  // and there is no stream of them the way there is of events within one. A
  // poll on a loopback socket is cheap, and a console that only ever shows
  // what existed when it opened is the wrong shape for a system whose whole
  // claim is that clients are projections.
  setInterval(() => {
    refreshSessions().catch((err) => setDaemonStatus(`could not list sessions — ${err.message}`));
  }, SESSION_POLL_MS);
}

function showLocked(message) {
  el('app').hidden = true;
  el('locked').hidden = false;
  if (message) {
    const error = el('unlock-error');
    error.textContent = message;
    error.hidden = false;
  }
}

// ----------------------------------------------------------------- sessions

async function refreshSessions() {
  const { sessions = [] } = await call('SessionService', 'ListSessions', {});

  // Rendering unconditionally would rebuild the list every few seconds and
  // take the keyboard focus with it.
  const changed = summarise(sessions) !== summarise(state.sessions);
  state.sessions = sessions;
  if (changed) renderSessions();

  if (!state.sessionId && sessions.length) {
    await selectSession(sessions[0].id);
  }
}

const summarise = (sessions) => sessions.map((s) => `${s.id}:${s.title}:${s.updatedAt}`).join('|');

function renderSessions() {
  const list = el('sessions');
  list.replaceChildren();

  for (const session of state.sessions) {
    const button = document.createElement('button');
    button.type = 'button';
    button.setAttribute('aria-current', String(session.id === state.sessionId));
    button.title = session.id;

    const name = document.createElement('span');
    name.textContent = session.title || '(untitled)';

    const when = document.createElement('span');
    when.className = 'when';
    when.textContent = formatWhen(session.updatedAt);

    button.append(name, when);
    button.addEventListener('click', () => selectSession(session.id));

    const item = document.createElement('li');
    item.append(button);
    list.append(item);
  }
}

function formatWhen(timestamp) {
  if (!timestamp) return '';
  const when = new Date(timestamp);
  const today = new Date().toDateString() === when.toDateString();
  return today
    ? when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : when.toLocaleDateString();
}

async function selectSession(id) {
  if (state.stream) state.stream.abort();

  state.sessionId = id;
  state.seq = 0;
  state.openMessage = null;
  state.approvals.clear();
  state.runId = null;
  setRunStatus('');

  el('timeline').replaceChildren();
  el('session-title').textContent = titleOf(id);
  el('input').disabled = false;
  el('send').disabled = false;
  renderSessions();
  renderApprovals();

  await loadApprovals();
  attach();
}

function titleOf(id) {
  const session = state.sessions.find((s) => s.id === id);
  return session?.title || id;
}

// attach follows the session's events, and keeps following.
//
// It resumes from the last sequence rendered rather than from the beginning,
// so a dropped connection costs a reconnect and not the whole history again.
function attach() {
  const controller = new AbortController();
  state.stream = controller;
  const forSession = state.sessionId;

  (async () => {
    for (;;) {
      try {
        setDaemonStatus('connected');
        await openStream(
          'SessionService',
          'SubscribeEvents',
          { sessionId: forSession, afterSeq: String(state.seq) },
          (frame) => {
            if (frame.event) applyEvent(frame.event);
          },
          controller.signal,
        );
      } catch (err) {
        if (controller.signal.aborted) return;
        setDaemonStatus(`reconnecting — ${err.message}`);
      }
      if (controller.signal.aborted) return;
      await sleep(1500);
      if (state.sessionId !== forSession) return;
    }
  })();
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// -------------------------------------------------------------------- events

function applyEvent(event) {
  // Sequences arrive as strings: protobuf JSON writes 64-bit integers that way
  // because a double cannot hold all of them.
  state.seq = Math.max(state.seq, Number(event.seq));

  if (event.userMessageAdded) {
    state.openMessage = null;
    return addLine('user', 'you', event.userMessageAdded.text);
  }

  if (event.assistantTextDelta) {
    return appendAssistant(event.assistantTextDelta.text || '');
  }

  if (event.assistantMessageCompleted) {
    state.openMessage = null;
    return;
  }

  if (event.runStateChanged) {
    const { status, reason } = event.runStateChanged;
    state.runId = event.runId || state.runId;
    setRunStatus(status, reason);
    state.openMessage = null;
    if (status === 'RUN_STATUS_FAILED' || status === 'RUN_STATUS_CANCELLED') {
      addLine('error', 'run', reason || status);
    }
    return;
  }

  if (event.toolCallRequested) {
    const { name, arguments: args } = event.toolCallRequested;
    state.openMessage = null;
    return addLine('tool', name, compact(args || '', 300));
  }

  if (event.toolCallCompleted) {
    const done = event.toolCallCompleted;
    const line = addLine(done.isError ? 'error' : 'tool', done.name,
      done.isError ? compact(done.content || '', 400) : (done.summary || 'ok'));
    if (done.artifact) line.append(artifactButton(done.artifact));
    return;
  }

  if (event.conversationCompacted) {
    const { messagesFolded, tokensBefore, tokensAfter } = event.conversationCompacted;
    return addLine('meta', 'compacted',
      `folded ${messagesFolded} messages, ~${tokensBefore} tokens to ~${tokensAfter}`);
  }

  if (event.approvalRequested) {
    const asked = event.approvalRequested;
    state.approvals.set(asked.approvalId, asked);
    return renderApprovals();
  }

  if (event.approvalResolved) {
    state.approvals.delete(event.approvalResolved.approvalId);
    return renderApprovals();
  }
}

function appendAssistant(text) {
  if (!state.openMessage) {
    state.openMessage = addLine('assistant', 'agent', '');
  }
  state.openMessage.textContent += text;
  scrollToEnd();
}

function addLine(kind, label, text) {
  const item = document.createElement('li');
  item.className = kind;

  const which = document.createElement('span');
  which.className = 'kind';
  which.textContent = label;

  const body = document.createElement('span');
  body.className = 'body';
  body.textContent = text;

  item.append(which, body);
  el('timeline').append(item);
  scrollToEnd();

  return body;
}

// artifactButton shows stored output where the reader already is.
//
// A new tab is the obvious idea and the wrong one: a popup can be blocked, a
// blob URL inherits a policy meant for this page, and the thing somebody
// actually wants to do with a build log is glance at it next to the line that
// produced it.
function artifactButton(artifact) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'artifact';
  button.textContent = `show ${formatBytes(artifact.size)}`;
  button.title = artifact.id;

  let panel = null;

  button.addEventListener('click', async () => {
    if (panel) {
      panel.remove();
      panel = null;
      button.textContent = `show ${formatBytes(artifact.size)}`;
      return;
    }

    button.disabled = true;
    button.textContent = 'loading…';

    try {
      const text = await readArtifact(artifact.id, ARTIFACT_VIEW_BYTES);

      panel = document.createElement('pre');
      panel.className = 'artifact-body';
      panel.textContent = text;
      if (Number(artifact.size) > ARTIFACT_VIEW_BYTES) {
        panel.textContent += `\n\n[showing the first ${formatBytes(ARTIFACT_VIEW_BYTES)} of ` +
          `${formatBytes(artifact.size)}; the whole of it is ${artifact.id}]`;
      }
      button.after(panel);
      button.textContent = 'hide';
    } catch (err) {
      button.textContent = `could not read: ${err.message}`;
    } finally {
      button.disabled = false;
    }
  });

  return button;
}

// readArtifact fetches stored bytes as text.
//
// A plain link cannot do it: the call is a POST and the credential belongs in
// a header rather than in a URL somebody might copy out of a screenshot.
async function readArtifact(id, limit) {
  const response = await fetch(`${API}ArtifactService/ReadArtifact`, {
    method: 'POST',
    headers: {
      'content-type': 'application/connect+json',
      'connect-protocol-version': '1',
      authorization: `Bearer ${state.token}`,
    },
    body: envelope({ meta: { clientId: CLIENT_ID }, id, offset: '0', limit: String(limit) }),
  });
  if (!response.ok) throw new Error(await response.text());

  const bytes = unframe(new Uint8Array(await response.arrayBuffer()));
  return new TextDecoder().decode(bytes);
}

// unframe concatenates the payloads of a fully-buffered Connect stream.
function unframe(framed) {
  const chunks = [];
  let at = 0;

  while (at + 5 <= framed.length) {
    const view = new DataView(framed.buffer, framed.byteOffset + at, 5);
    const flags = view.getUint8(0);
    const length = view.getUint32(1);
    const start = at + 5;
    at = start + length;

    if (flags & 0x02) break;

    const message = JSON.parse(new TextDecoder().decode(framed.subarray(start, at)));
    if (message.chunk) chunks.push(base64ToBytes(message.chunk));
  }

  return chunks.reduce(concat, new Uint8Array(0));
}

// Protobuf JSON writes bytes as base64, so they arrive as a string.
function base64ToBytes(encoded) {
  const binary = atob(encoded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

// ------------------------------------------------------------------ approvals

async function loadApprovals() {
  try {
    const { approvals = [] } = await call('SessionService', 'ListApprovals', {
      sessionId: state.sessionId,
    });
    state.approvals.clear();
    for (const approval of approvals) state.approvals.set(approval.approvalId, approval);
    renderApprovals();
  } catch (err) {
    setDaemonStatus(`could not read approvals — ${err.message}`);
  }
}

function renderApprovals() {
  const panel = el('approvals');
  panel.replaceChildren();
  panel.hidden = state.approvals.size === 0;

  for (const approval of state.approvals.values()) {
    const row = document.createElement('div');
    row.className = 'approval';

    const what = document.createElement('div');
    what.className = 'what';

    const tool = document.createElement('div');
    tool.className = 'tool mono';
    tool.textContent = approval.toolName;

    const args = document.createElement('div');
    args.className = 'args mono';
    args.textContent = compact(approval.arguments || '', 240);

    what.append(tool, args);

    if (approval.effects?.length) {
      const effects = document.createElement('div');
      effects.className = 'effects';
      effects.textContent = approval.effects.join(' · ');
      what.append(effects);
    }

    row.append(what,
      decideButton(approval, 'APPROVAL_DECISION_ALLOW', 'REMEMBER_SCOPE_ONCE', 'Allow', 'primary'),
      decideButton(approval, 'APPROVAL_DECISION_ALLOW', 'REMEMBER_SCOPE_SESSION', 'Allow for session', ''),
      decideButton(approval, 'APPROVAL_DECISION_DENY', 'REMEMBER_SCOPE_ONCE', 'Deny', 'danger'));

    panel.append(row);
  }
}

function decideButton(approval, decision, remember, label, className) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = className;
  button.textContent = label;

  button.addEventListener('click', async () => {
    button.disabled = true;
    try {
      await call('SessionService', 'DecideApproval', {
        approvalId: approval.approvalId,
        decision,
        remember,
      });
      // The resolution arrives as an event too; removing it here keeps the
      // panel from sitting there looking unanswered in the meantime.
      state.approvals.delete(approval.approvalId);
      renderApprovals();
    } catch (err) {
      button.disabled = false;
      setDaemonStatus(`could not decide — ${err.message}`);
    }
  });

  return button;
}

// ------------------------------------------------------------------- sending

async function send(text) {
  const input = el('input');
  input.value = '';

  try {
    const { runId } = await call('SessionService', 'SendTurn', {
      sessionId: state.sessionId,
      text,
      requestId: crypto.randomUUID(),
    });
    state.runId = runId;
  } catch (err) {
    input.value = text;
    setDaemonStatus(`could not send — ${err.message}`);
  }
}

function setRunStatus(status, reason) {
  state.runStatus = status;
  const readable = status ? status.replace('RUN_STATUS_', '').toLowerCase() : '';
  el('run-status').textContent = reason ? `${readable}: ${reason}` : readable;

  const running = status === 'RUN_STATUS_RUNNING';
  el('interrupt').hidden = !running;
}

function setDaemonStatus(text) {
  el('daemon-status').textContent = text;
}

function compact(text, limit) {
  const oneLine = text.replace(/\s+/g, ' ').trim();
  return oneLine.length > limit ? `${oneLine.slice(0, limit)}…` : oneLine;
}

function formatBytes(size) {
  const bytes = Number(size || 0);
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function scrollToEnd() {
  const timeline = el('timeline');
  // Only when they were already at the bottom: yanking the view away from
  // somebody reading back through a long run is worse than a missed line.
  const atBottom = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 80;
  if (atBottom) timeline.scrollTop = timeline.scrollHeight;
}

// --------------------------------------------------------------------- wiring

el('unlock').addEventListener('submit', async (event) => {
  event.preventDefault();
  el('unlock-error').hidden = true;

  try {
    await useToken(await redeem(el('code').value.trim()));
    location.reload();
  } catch (err) {
    showLocked(err.message);
  }
});

el('new-session').addEventListener('click', async () => {
  const { session } = await call('SessionService', 'CreateSession', { title: '' });
  await refreshSessions();
  await selectSession(session.id);
  el('input').focus();
});

el('composer').addEventListener('submit', (event) => {
  event.preventDefault();
  const text = el('input').value.trim();
  if (text && state.sessionId) send(text);
});

el('input').addEventListener('keydown', (event) => {
  if (event.key === 'Enter' && !event.shiftKey) {
    event.preventDefault();
    el('composer').requestSubmit();
  }
});

el('interrupt').addEventListener('click', async () => {
  if (!state.runId) return;
  try {
    await call('SessionService', 'InterruptRun', {
      runId: state.runId,
      reason: 'interrupted from the web console',
    });
  } catch (err) {
    setDaemonStatus(`could not interrupt — ${err.message}`);
  }
});

start().catch((err) => showLocked(err.message));
