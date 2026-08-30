package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// stream turns the service's event stream into canonical events.
//
// The shape is a state machine rather than a chain of deltas: a content block
// is opened, added to, and stopped, and an index says which one is being
// spoken about. A tool call's arguments arrive as fragments of JSON across
// several deltas, so they are assembled here — a half-parsed call cannot be
// validated, let alone run.
type stream struct {
	body    io.ReadCloser
	lines   *bufio.Scanner
	pending []provider.Event

	// building holds the tool calls being assembled, by the index the service
	// is using for them.
	building map[int]*partialCall

	usage  domain.Usage
	closed bool
}

type partialCall struct {
	id   string
	name string
	args strings.Builder
}

func newStream(body io.ReadCloser) *stream {
	lines := bufio.NewScanner(body)

	// A single event can carry a whole tool call's arguments, and the default
	// is a line long enough for prose and not for that.
	lines.Buffer(make([]byte, 0, 64<<10), 4<<20)

	return &stream{body: body, lines: lines, building: make(map[int]*partialCall)}
}

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

// Recv returns the next canonical event.
func (s *stream) Recv(ctx context.Context) (provider.Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(s.pending) > 0 {
			next := s.pending[0]
			s.pending = s.pending[1:]
			return next, nil
		}
		if !s.lines.Scan() {
			if err := s.lines.Err(); err != nil {
				return nil, fmt.Errorf("anthropic: read the stream: %w", err)
			}
			return nil, io.EOF
		}

		line := strings.TrimSpace(s.lines.Text())

		// Only the data lines carry anything. The event: lines name what
		// follows, and the name is also in the payload.
		data, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, fmt.Errorf("anthropic: unreadable stream event: %w", err)
		}
		if err := s.consume(event); err != nil {
			return nil, err
		}
	}
}

// consume turns one wire event into zero or more canonical ones.
func (s *stream) consume(event streamEvent) error {
	switch event.Type {
	case "message_start":
		if event.Message != nil && event.Message.Usage != nil {
			s.take(*event.Message.Usage)
		}

	case "content_block_start":
		if event.ContentBlock == nil {
			return nil
		}
		if event.ContentBlock.Type == "tool_use" {
			s.building[event.Index] = &partialCall{
				id:   event.ContentBlock.ID,
				name: event.ContentBlock.Name,
			}
		}

	case "content_block_delta":
		if event.Delta == nil {
			return nil
		}
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				s.pending = append(s.pending, provider.TextDelta{Text: event.Delta.Text})
			}

		case "thinking_delta":
			// Its own event, and the separation is the point: this is
			// working-out rather than what the model is telling anybody, and
			// folded into the answer it would be posted wherever the answer
			// goes.
			if event.Delta.Thinking != "" {
				s.pending = append(s.pending, provider.ReasoningDelta{Text: event.Delta.Thinking})
			}

		case "input_json_delta":
			if call, building := s.building[event.Index]; building {
				call.args.WriteString(event.Delta.PartialJSON)
			}
		}

	case "content_block_stop":
		if call, building := s.building[event.Index]; building {
			delete(s.building, event.Index)
			s.pending = append(s.pending, provider.ToolCallRequested{
				ID:   call.id,
				Name: call.name,
				Args: arguments(call.args.String()),
			})
		}

	case "message_delta":
		if event.Usage != nil {
			s.take(*event.Usage)
		}
		if event.Delta != nil && event.Delta.StopReason != "" {
			s.pending = append(s.pending, provider.Completed{
				StopReason: stopReason(event.Delta.StopReason),
				RawReason:  event.Delta.StopReason,
			})
		}

	case "error":
		if event.Error != nil {
			return &provider.Error{
				Provider: "anthropic",
				Kind:     errorKind(event.Error.Type),
				Message:  event.Error.Message,
			}
		}
		return errors.New("anthropic: the stream reported an error with no detail")
	}

	return nil
}

// take records usage as the latest known total.
func (s *stream) take(reported usage) {
	if reported.InputTokens > 0 {
		s.usage.InputTokens = reported.InputTokens
	}
	if reported.OutputTokens > 0 {
		s.usage.OutputTokens = reported.OutputTokens
	}
	if reported.CacheReadInputTokens > 0 {
		s.usage.CachedInputTokens = reported.CacheReadInputTokens
	}
	s.pending = append(s.pending, provider.UsageDelta{Usage: s.usage})
}

// arguments is a tool call's input, as valid JSON.
//
// A call with no arguments sends nothing at all rather than "{}", and an
// empty string is not a document: passed on it would fail validation as
// malformed rather than as missing a field.
func arguments(assembled string) json.RawMessage {
	if strings.TrimSpace(assembled) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(assembled)
}

// stopReason maps what the service said onto what the runtime knows.
//
// The raw value travels beside this. The set is not closed in practice, and
// an adapter that has to choose between the known values will pick one — "it
// stopped normally" being a plausible, wrong and unfalsifiable answer for a
// generation that was cut off.
func stopReason(said string) domain.StopReason {
	switch said {
	case "end_turn", "stop_sequence":
		return domain.StopEndTurn
	case "max_tokens":
		return domain.StopMaxTokens
	case "tool_use":
		return domain.StopToolUse
	case "refusal":
		return domain.StopContentFilter
	default:
		return domain.StopEndTurn
	}
}

// errorKind reads an error the service reported inside the stream.
func errorKind(said string) provider.ErrorKind {
	switch said {
	case "overloaded_error":
		return provider.KindOverloaded
	case "rate_limit_error":
		return provider.KindRateLimited
	case "authentication_error", "permission_error":
		return provider.KindAuth
	case "not_found_error":
		return provider.KindNotFound
	case "invalid_request_error":
		return provider.KindInvalidRequest
	default:
		return provider.KindUnknown
	}
}
