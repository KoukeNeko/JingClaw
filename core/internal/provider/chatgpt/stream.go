package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// stream turns the Responses event stream into canonical events.
//
// A response succeeded only if response.completed arrived. A stream that ends
// without it was interrupted, and an incomplete or failed one is an error: the
// documentation is explicit that anything else is not success.
type stream struct {
	body    io.ReadCloser
	lines   *bufio.Scanner
	tokens  TokenSource
	pending []provider.Event

	calledTool bool
	finished   bool
	closed     bool
}

func newStream(body io.ReadCloser, tokens TokenSource) *stream {
	lines := bufio.NewScanner(body)
	// One event can carry a whole tool call's arguments.
	lines.Buffer(make([]byte, 0, 64<<10), 8<<20)
	return &stream{body: body, lines: lines, tokens: tokens}
}

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

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
		if s.finished {
			return nil, io.EOF
		}
		if !s.lines.Scan() {
			// Stopped by the caller giving up (shutdown, deadline): cancelled,
			// not a failed connection.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Ended with no response.completed, whatever the reason.
			cause := "the connection ended before ChatGPT finished the response"
			if err := s.lines.Err(); err != nil {
				cause += ": " + err.Error()
			}
			return nil, &provider.Error{Provider: providerName, Kind: provider.KindTransient, Message: cause}
		}

		data, found := strings.CutPrefix(strings.TrimSpace(s.lines.Text()), "data:")
		data = strings.TrimSpace(data)
		if !found || data == "" || data == "[DONE]" {
			continue
		}

		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, fmt.Errorf("chatgpt: unreadable stream event: %w", err)
		}
		if err := s.consume(event); err != nil {
			return nil, err
		}
	}
}

func (s *stream) consume(event streamEvent) error {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			s.pending = append(s.pending, provider.TextDelta{Text: event.Delta})
		}

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// Working-out, kept apart from the answer. Only present if the service
		// sends it; nothing here asks for it.
		if event.Delta != "" {
			s.pending = append(s.pending, provider.ReasoningDelta{Text: event.Delta})
		}

	case "response.output_item.done":
		// The finished item carries the complete arguments, so the argument
		// fragments that precede it are not assembled here.
		if item := event.Item; item != nil && item.Type == "function_call" {
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			s.calledTool = true
			s.pending = append(s.pending, provider.ToolCallRequested{
				ID: item.CallID, Name: item.Name, Args: json.RawMessage(args),
			})
		}

	case "response.completed":
		if response := event.Response; response != nil && response.Usage != nil {
			u := response.Usage
			s.pending = append(s.pending, provider.UsageDelta{Usage: domain.Usage{
				InputTokens:       u.InputTokens,
				CachedInputTokens: u.InputTokenDetails.CachedTokens,
				OutputTokens:      u.OutputTokens,
				ReasoningTokens:   u.OutputTokenDetails.ReasoningTokens,
			}})
		}
		stop := domain.StopEndTurn
		if s.calledTool {
			stop = domain.StopToolUse
		}
		s.pending = append(s.pending, provider.Completed{StopReason: stop, RawReason: "completed"})
		s.finished = true
		s.tokens.RecordError("", "")

	case "response.incomplete":
		reason := ""
		if event.Response != nil && event.Response.IncompleteDetails != nil {
			reason = event.Response.IncompleteDetails.Reason
		}
		kind := provider.KindUnknown
		if reason == "content_filter" {
			kind = provider.KindContentFiltered
		}
		return &provider.Error{Provider: providerName, Kind: kind,
			Message: "ChatGPT ended the response early (" + cleanReason(reason) + ")"}

	case "response.failed":
		if response := event.Response; response != nil && response.Error != nil {
			return s.report(response.Error.Code, response.Error.Param, response.Error.Message)
		}
		return &provider.Error{Provider: providerName, Kind: provider.KindUnknown,
			Message: "ChatGPT reported the response failed, with no detail"}

	case "error":
		return s.report(event.Code, event.Param, event.Message)
	}
	return nil
}

// report maps an error that arrived inside the stream, and remembers its code.
func (s *stream) report(code, param, message string) error {
	perr := failure(code, param, message, 0)
	s.tokens.RecordError(code, perr.Message)
	return perr
}

func cleanReason(reason string) string {
	if reason == "" {
		return "no reason given"
	}
	if len(reason) > 64 {
		reason = reason[:64]
	}
	return reason
}
