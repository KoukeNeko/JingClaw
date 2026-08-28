package openaicompat

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, raw string) []sseEvent {
	t.Helper()

	reader := newSSEReader(strings.NewReader(raw))

	var events []sseEvent
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		events = append(events, event)
	}
}

// The protocol allows more shapes than the one servers usually send, and a
// decoder that only handles the usual one fails on a proxy that rewrites line
// endings or a server that pads with comments.
func TestTheDecoderAcceptsWhatTheProtocolAllows(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "the ordinary shape",
			raw:  "data: one\n\ndata: two\n\n",
			want: []string{"one", "two"},
		},
		{
			// A proxy that normalizes to CRLF, which is legal and common.
			name: "carriage returns",
			raw:  "data: one\r\n\r\ndata: two\r\n\r\n",
			want: []string{"one", "two"},
		},
		{
			// Several data lines join with newlines rather than replacing
			// each other.
			name: "an event split across data lines",
			raw:  "data: {\"a\":\ndata: 1}\n\n",
			want: []string{"{\"a\":\n1}"},
		},
		{
			// Heartbeats, sent to hold a connection open.
			name: "comments between events",
			raw:  ": keep-alive\ndata: one\n\n: keep-alive\n\ndata: two\n\n",
			want: []string{"one", "two"},
		},
		{
			// The space after the colon is syntax, not content; but a value
			// with no space is equally valid.
			name: "no space after the colon",
			raw:  "data:one\n\n",
			want: []string{"one"},
		},
		{
			// Fields this protocol has no use for must not derail it.
			name: "id and retry fields",
			raw:  "id: 7\nretry: 100\ndata: one\n\n",
			want: []string{"one"},
		},
		{
			// A stream cut off before its final blank line. The last event is
			// complete and discarding it would lose an answer.
			name: "no trailing blank line",
			raw:  "data: one\n\ndata: two\n",
			want: []string{"one", "two"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := readAll(t, test.raw)

			if len(events) != len(test.want) {
				t.Fatalf("got %d events, want %d: %+v", len(events), len(test.want), events)
			}
			for i, want := range test.want {
				if events[i].Data != want {
					t.Errorf("event %d is %q, want %q", i, events[i].Data, want)
				}
			}
		})
	}
}

// The reason this is not built on a scanner: a scanner refuses any token over
// its buffer and reports it indistinguishably from a broken connection. The
// events that exceed it are the interesting ones.
func TestAnEventLargerThanTheBufferIsRead(t *testing.T) {
	huge := strings.Repeat("x", 512*1024)

	events := readAll(t, "data: "+huge+"\n\n")

	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	if len(events[0].Data) != len(huge) {
		t.Errorf("read %d bytes, want %d", len(events[0].Data), len(huge))
	}
}

// Bounded all the same. A server that never sends a blank line would
// otherwise fill this process's memory.
func TestAnUnboundedEventIsRefused(t *testing.T) {
	endless := strings.NewReader("data: " + strings.Repeat("x", maxEventBytes+1024))

	if _, err := newSSEReader(endless).Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("an oversized event returned %v, want ErrEventTooLarge", err)
	}
}
