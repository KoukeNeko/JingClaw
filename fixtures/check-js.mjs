// Checks the console's reducer against the shared fixtures.
//
// Run from the repository root:
//
//   node fixtures/check-js.mjs
//
// A disagreement here means the console draws a session differently from the
// daemon and from the other clients, which is the drift the fixtures exist to
// catch.

import { readFileSync } from 'node:fs';
import { reduceAll } from '../core/internal/webui/assets/reduce.js';

const { cases } = JSON.parse(readFileSync(new URL('./session-view.json', import.meta.url)));

// The fixtures carry each event body as JSON text, so that a language reading
// them does not have to know every payload shape in advance.
const parsed = (events) => events.map((e) => ({
  seq: e.seq,
  kind: e.kind,
  body: typeof e.body === 'string' ? JSON.parse(e.body) : e.body,
}));

// Compared as normalised JSON rather than field by field: a missing field and
// a field set to its zero value are the same screen, and a check that tells
// them apart fails for reasons nobody can act on.
const normalise = (state) => JSON.stringify({
  messages: (state.messages || []).map((m) => ({
    role: m.role,
    text: m.text || '',
    reasoning: m.reasoning || '',
    tool_calls: (m.tool_calls || []).map((c) => ({
      name: c.name, completed: !!c.completed, is_error: !!c.is_error,
    })),
  })),
  pending_approvals: state.pending_approvals || [],
  active_run: state.active_run || '',
  head_seq: Number(state.head_seq || 0),
});

// The normaliser above names the fields it compares, so a field added to the
// fixtures and not to it is silently exempt — every client would pass without
// computing it. Checked rather than remembered: this is exactly the drift the
// fixtures exist to catch, and it would be invisible in the one place looking
// for drift.
const KNOWN_STATE_KEYS = ['messages', 'pending_approvals', 'active_run', 'head_seq'];
const KNOWN_MESSAGE_KEYS = ['role', 'text', 'reasoning', 'tool_calls'];
const KNOWN_CALL_KEYS = ['name', 'completed', 'is_error'];

const unknownKeys = (object, known) => Object.keys(object || {}).filter((k) => !known.includes(k));

const uncompared = (expected) => {
  const missed = unknownKeys(expected, KNOWN_STATE_KEYS);
  for (const message of expected.messages || []) {
    missed.push(...unknownKeys(message, KNOWN_MESSAGE_KEYS));
    for (const call of message.tool_calls || []) {
      missed.push(...unknownKeys(call, KNOWN_CALL_KEYS));
    }
  }
  return missed;
};

let failed = 0;
for (const testCase of cases) {
  const missed = uncompared(testCase.expected);
  if (missed.length > 0) {
    failed += 1;
    console.error(`FAIL  ${testCase.name}`);
    console.error(`      the fixture carries ${missed.join(', ')}, which this check does not compare;`);
    console.error('      add it to the normaliser and to every client, or the field is checked nowhere');
    continue;
  }

  const got = reduceAll(parsed(testCase.events));
  if (normalise(got) !== normalise(testCase.expected)) {
    failed += 1;
    console.error(`FAIL  ${testCase.name}`);
    console.error(`      ${testCase.why}`);
    console.error(`      got:  ${normalise(got)}`);
    console.error(`      want: ${normalise(testCase.expected)}`);
  } else {
    console.log(`ok    ${testCase.name}`);
  }
}

if (failed > 0) {
  console.error(`\n${failed} of ${cases.length} cases disagree`);
  process.exit(1);
}
console.log(`\nall ${cases.length} cases agree`);
