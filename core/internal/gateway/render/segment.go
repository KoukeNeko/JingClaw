package render

import (
	"encoding/json"
	"fmt"
	"strings"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// SegmentKind says what a piece of an answer is.
type SegmentKind int

const (
	// SegmentText is prose, code, a list — anything that goes in a message.
	SegmentText SegmentKind = iota

	// SegmentTable is a table, to be drawn rather than written out.
	SegmentTable
)

// Segment is one piece of an answer, in the order it was written.
//
// An answer with a table in the middle of it is three things — what was said
// before, the table, and what was said after — and a platform that cannot put
// a picture inside a message has to send three. Their order is the answer's
// order: a table that arrived after the sentence explaining it would read as
// being about something else.
type Segment struct {
	Kind  SegmentKind
	Text  string
	Table Table
}

// Segments splits text at its tables.
//
// Text with no table is one segment, which is what every caller that does not
// draw pictures gets and is why they need no special case.
//
// Empty stretches between two tables are dropped: what they would produce is
// a message with nothing in it, which platforms refuse and readers would not
// want if they did not.
func Segments(text string) []Segment {
	found := Tables(text)
	if len(found) == 0 {
		return []Segment{{Kind: SegmentText, Text: text}}
	}

	var segments []Segment
	written := 0

	add := func(piece string) {
		if strings.TrimSpace(piece) == "" {
			return
		}
		segments = append(segments, Segment{Kind: SegmentText, Text: strings.TrimSpace(piece)})
	}

	for _, at := range found {
		add(text[written:at.Start])
		segments = append(segments, Segment{Kind: SegmentTable, Table: at.Table})
		written = at.End
	}
	add(text[written:])

	return segments
}

// DispatchSegments is Dispatch for a platform that draws tables rather than
// writing them out.
//
// Only an answer is split. A question, an approval and a log line are already
// short and shaped by what they are; a table in one of them would be a table
// somebody put there by hand, and turning it into a picture would take the
// controls off the message that needs them.
func DispatchSegments(dispatch jcgateway.Dispatch, style Style) ([]Segment, error) {
	if dispatch.Kind != jcgateway.DispatchMessage {
		body, err := Dispatch(dispatch, style)
		if err != nil {
			return nil, err
		}
		return []Segment{{Kind: SegmentText, Text: body}}, nil
	}

	var payload jcgateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return nil, fmt.Errorf("render: could not decode message payload: %w", err)
	}
	return Segments(NormalizeText(payload.Text)), nil
}
