package openaicompat

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxEventBytes bounds one server-sent event.
//
// Generous, because a single event carries a whole tool call and tool
// arguments are routinely large. Bounded all the same: without a ceiling, a
// server that never sends a blank line is a server that fills this process's
// memory.
const maxEventBytes = 8 << 20

// ErrEventTooLarge is returned for an event over the ceiling.
var ErrEventTooLarge = errors.New("openaicompat: a server-sent event exceeded the size limit")

// sseEvent is one dispatched event.
type sseEvent struct {
	// Name is the event: field. Most of these streams never set it.
	Name string

	// Data is every data: line joined by newlines, as the protocol specifies.
	Data string
}

// sseReader decodes a server-sent event stream.
//
// Written against bufio.Reader rather than bufio.Scanner on purpose. A Scanner
// refuses any token over its buffer — 64KiB by default — and reports it as an
// error indistinguishable from a broken connection. The events that exceed it
// are exactly the interesting ones: a large tool call, a structured output.
type sseReader struct {
	reader *bufio.Reader
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{reader: bufio.NewReaderSize(body, 64*1024)}
}

// Next returns the next dispatched event, or io.EOF at the end of the stream.
func (r *sseReader) Next() (sseEvent, error) {
	var (
		event sseEvent
		data  []string
		size  int
		seen  bool
	)

	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && seen {
				// A stream that ended without a final blank line. The event
				// is complete; discarding it would lose the last answer.
				return finish(event, data), nil
			}
			return sseEvent{}, err
		}

		size += len(line)
		if size > maxEventBytes {
			return sseEvent{}, ErrEventTooLarge
		}

		// A line beginning with a colon is a comment, and several servers
		// send them as heartbeats. Checked before anything is considered
		// started: a heartbeat followed by a blank line is not an event, and
		// treating it as one hands the caller a frame with no data in it.
		if len(line) > 0 && line[0] == ':' {
			continue
		}

		// A blank line dispatches whatever has accumulated.
		if len(line) == 0 {
			if !seen {
				continue
			}
			return finish(event, data), nil
		}
		seen = true

		field, value := splitField(line)
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			event.Name = value
		case "id", "retry":
			// Accepted and ignored: this protocol has no use for resuming a
			// generation from an offset.
		}
	}
}

func finish(event sseEvent, data []string) sseEvent {
	event.Data = strings.Join(data, "\n")
	return event
}

// splitField parses "field: value", where the single leading space after the
// colon is part of the syntax rather than of the value.
func splitField(line string) (field, value string) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		// A field with no value, which the protocol allows.
		return line, ""
	}

	field = line[:colon]
	value = line[colon+1:]
	return field, strings.TrimPrefix(value, " ")
}

// readLine reads one line, accepting every line ending the protocol allows and
// without the ceiling a scanner would impose.
func (r *sseReader) readLine() (string, error) {
	var full []byte

	for {
		chunk, err := r.reader.ReadSlice('\n')
		full = append(full, chunk...)

		switch {
		case err == nil:
			return string(trimEnding(full)), nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(full) > maxEventBytes {
				return "", ErrEventTooLarge
			}
			continue
		case errors.Is(err, io.EOF):
			if len(full) > 0 {
				return string(trimEnding(full)), nil
			}
			return "", io.EOF
		default:
			return "", fmt.Errorf("openaicompat: reading the stream: %w", err)
		}
	}
}

func trimEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}
