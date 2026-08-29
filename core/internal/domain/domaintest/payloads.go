// Package domaintest supplies one sample of every event payload.
//
// It exists so that the packages which have to understand every kind — storage
// and the wire format — can prove they do, against a single list rather than
// against whatever the author of each test happened to remember. Adding a kind
// without adding it here fails a test in both.
package domaintest

import (
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Payloads returns a sample of each event payload, keyed by its kind.
//
// The values are filled in rather than left zero: a round trip that only ever
// carries empty strings proves nothing about the fields.
func Payloads() map[domain.EventKind]domain.EventPayload {
	return map[domain.EventKind]domain.EventPayload{
		domain.EventUserMessageAdded: domain.UserMessageAdded{
			MessageID: "msg_1",
			Text:      "修掉 failing test",
			Trust:     domain.TrustUser,
			Origin: domain.RunOrigin{
				Kind:     domain.OriginLocalClient,
				ClientID: "jingclaw-cli",
			},
			Attachments: []domain.Attachment{{
				ArtifactID: "sha256-" + strings.Repeat("cd", 32),
				Name:       "screenshot.png",
				MediaType:  "image/png",
				Size:       48_213,
			}},
		},
		domain.EventRunStateChanged: domain.RunStateChanged{
			Status:      domain.RunFailed,
			Reason:      "provider unavailable",
			FailureKind: "overloaded",
		},
		domain.EventAssistantTextDelta: domain.AssistantTextDelta{
			MessageID: "msg_2",
			Text:      "收到：",
		},
		domain.EventAssistantReasoningDelta: domain.AssistantReasoningDelta{
			MessageID: "msg_2",
			Text:      "使用者問的是時間，先確認時區。",
		},
		domain.EventAssistantMessageCompleted: domain.AssistantMessageCompleted{
			MessageID:  "msg_2",
			StopReason: domain.StopEndTurn,
		},
		domain.EventUsageChanged: domain.UsageChanged{
			Usage: domain.Usage{
				InputTokens:       1200,
				CachedInputTokens: 900,
				OutputTokens:      340,
				ReasoningTokens:   80,
			},
		},
		domain.EventToolCallRequested: domain.ToolCallRequested{
			CallID:           "call_1",
			Name:             "read_file",
			Arguments:        `{"path":"main.go"}`,
			ProviderMetadata: `{"thought_signature":"abc"}`,
		},
		domain.EventToolCallCompleted: domain.ToolCallCompleted{
			CallID:    "call_1",
			Name:      "read_file",
			Summary:   "read main.go",
			Content:   "package main",
			IsError:   true,
			Truncated: true,
			Artifact: &domain.Artifact{
				ID:        "sha256-" + strings.Repeat("ab", 32),
				Size:      120_000,
				MediaType: "text/plain",
			},
			DurationMS: 42,
		},
		domain.EventConversationCompacted: domain.ConversationCompacted{
			Summary:        "The user asked for the failing test to be fixed.",
			ThroughSeq:     17,
			MessagesFolded: 9,
			TokensBefore:   120_000,
			TokensAfter:    30_000,
		},
		domain.EventRunDirections: domain.RunDirections{
			Text: "Standing directions you were given in earlier sessions:\n\n" +
				"- prefer table-driven tests",
		},
		domain.EventApprovalRequested: domain.ApprovalRequested{
			ApprovalID: "apr_1",
			CallID:     "call_2",
			ToolName:   "exec_command",
			Arguments:  `{"program":"git","args":["push"]}`,
			Summary:    "git push",
			Effects:    []string{"network", "destructive"},
		},
		domain.EventApprovalResolved: domain.ApprovalResolved{
			ApprovalID: "apr_1",
			CallID:     "call_2",
			ToolName:   "exec_command",
			Status:     domain.ApprovalAllowed,
			Scope:      domain.RememberSession,
			DecidedBy:  "cli",
		},
	}
}
