# JingClaw in a chat channel

How a conversation reaches the agent, what it shows while it works, and how an answer comes back.

## What it is doing, on Discord

A run that reads four files and waits on a test suite used to say nothing at
all between "working on it" and the answer, and silence because it is busy
looks exactly like silence because something broke. It now says what it is
doing:

```
江委員  _Working on it — `read_file notes.txt`_
```

That line is **rewritten in place** rather than added to. A run touching ten
files leaves one line behind it, not ten, and when the run ends the same line
becomes `_Done in 12s._` instead of still claiming to be busy.

It belongs to its run. Keyed by channel — which it was, briefly — a new run
edited the line the previous one left behind, and since that line had ended up
at the bottom of the previous answer, asking a fresh question rewrote the tail
of the last one.

The lines are throttled to `gateway.working_interval` (two seconds), which is
about as fast as anybody reads and comfortably inside what Discord accepts as
edits to one message. Which message is live is held in the adapter's memory
rather than in the outbox, because it is a presentation detail: losing it
across a restart costs one extra line in a channel, not a wrong one.

## Waiting in line, and taking a message back

A session answers one message at a time. A second message sent while the
first is still being answered gets 📥 on it: received, and waiting its turn.
The mark comes off the moment its turn comes.

**Pressing that 📥 yourself takes the message back.** It comes out of the
line at once, the bot swaps its 📥 for 🚮, and nothing is posted — the
person who took it back is the one who would have read a line about it. The
model is never shown the message; the log keeps it. Only the sender's press
counts: anybody else's 📥 on the message does nothing at all.

It is the waiting mark and not deleting the message, because people delete
messages for many reasons — wrong channel, a typo they are about to fix — and
not every one of them means "do not answer that". And it is not a way to stop
an answer already being written: a message that has started has no 📥 to
press, and stopping it is `interrupt` at a console, said deliberately.

The bot needs no new permission for this beyond adding reactions, which it
already does; it listens for reactions on the messages it marked, which is a
standard intent and not a privileged one.

## A private console channel

A channel bound as a console (`[[gateway.discord.consoles]]`) is the terminal
console for somebody not at the machine. Every run in the deployment shows up
there line by line, whichever room it is happening in:

```
-# `#ses_01M1` **MESSAGE** `gateway:doeshing` 那來輕鬆一點
-# `#ses_01M1` **RUN** running
-# `#ses_01M1` **TOOL** → `mcp_zhtw_zhtw` {"explain":true,"text":"報告江董…"}
-# `#ses_01M1` **TOOL** ✓ `mcp_zhtw_zhtw` mcp_zhtw_zhtw: ok
-# `#ses_01M1` **ANSWER** end_turn
-# `#ses_01M1` **RUN** completed
```

The same lines the terminal draws, because the same code draws them. A
finished call carries what the tool printed, in a code block, bounded. The
room the run is happening in sees its answer and its working line, and none
of this.

## Answers as they are written

A model writing for twenty seconds used to be twenty seconds of nothing
followed by a wall of text. The answer now grows in one message: the projector
sends what has been said so far on a cadence, each version naming the answer it
belongs to, and the adapter rewrites the message rather than posting the same
paragraph again.

The first delta only starts the clock, so an answer finished inside one
`gateway.stream_interval` (1.5 seconds) never streams — it arrives whole, which
is what it should do rather than appearing and being rewritten a moment later.

While it is being written an answer occupies exactly one message, cut at
Discord's limit with an ellipsis. Deciding between several messages and a file
belongs to the final version, when the whole thing is known.

## Long answers on Discord

A reply that needs eight messages is a channel somebody has to scroll past for
the rest of the day. Past `gateway.max_messages` (three by default) the answer
goes as a `.txt` attachment instead, with its opening in the message so a
reader can tell whether it is worth opening:

```
Here is what I found. The suite fails in three places, all in the
vowel counting…

the whole answer is attached (18.4 KB)
```

Only the agent's own answers. An approval is the thing somebody has to act on
and a status line is short by construction; hiding either behind a download
would be worse than a long channel.

No link is ever generated for it. A `http://127.0.0.1:PORT/…` address means
nothing to anybody reading Discord, and the artifact store is not something
this daemon publishes.
