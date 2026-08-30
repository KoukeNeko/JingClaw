package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// telegramStyle is what this platform can show.
//
// Plain text, deliberately. Telegram's parse modes reject a message whose
// entities do not balance, and a model writes an unmatched asterisk often
// enough that formatting would mean occasionally posting nothing at all.
// Losing bold is cheaper than losing an answer.
//
// Nothing marks a subdued line, because Telegram has no small text. A run
// summary is simply the lines after the answer.
var telegramStyle = render.Style{
	MaxLength:   maxMessageLength,
	SoftLength:  softMessageLength,
	WorkingLine: true,

	// No fence and no table width: this adapter sends plain text, so a
	// monospaced block would show its own backticks. Tables become rows.
	TableColumns: 0,
}

const (
	// Telegram refuses a message body longer than this, counted in UTF-16
	// code units rather than bytes.
	maxMessageLength = 4096

	// Cut below the hard limit so a reopened code fence or a continuation
	// marker still fits.
	softMessageLength = 3900
)

// Post delivers one dispatch and returns the ids Telegram gave the messages.
func (a *Adapter) Post(ctx context.Context, dispatch jcgateway.Dispatch) ([]string, error) {
	chatID, err := strconv.ParseInt(targetChat(dispatch.Target), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram: unusable chat in dispatch %s: %w", dispatch.ID, err)
	}

	body, err := render.Dispatch(dispatch, telegramStyle)
	if err != nil {
		return nil, err
	}

	if dispatch.Kind == jcgateway.DispatchStatus {
		return a.postStatus(ctx, chatID, dispatch, body)
	}
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}

	if file, carried := attachedFile(dispatch); carried {
		return a.postFile(ctx, chatID, file)
	}

	return a.postSegments(ctx, chatID, render.Split(body, telegramStyle))
}

// postStatus keeps one status line per run and rewrites it.
//
// Telegram has reactions, but only a fixed set of emoji and only one per
// message from a bot, so "running" and "finished" cannot both be shown that
// way. An edited message says more and does not silently drop a state the
// platform has no symbol for.
func (a *Adapter) postStatus(
	ctx context.Context,
	chatID int64,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}

	run := domain.RunID(dispatch.RunID)
	if existing, showing := a.liveStatus(run); showing {
		edited, err := a.edit(ctx, chatID, existing, body)
		if err == nil {
			if isFinalStatus(dispatch.Payload) {
				a.clearStatus(run)
			}
			return []string{strconv.FormatInt(edited, 10)}, nil
		}
		// A message somebody deleted, or one from before this process
		// started. Saying what the run is doing matters more than reusing
		// the line it used to say it in.
		a.config.Logger.Warn("could not edit the status line, posting a new one",
			"chat_id", chatID, "error", err)
		a.clearStatus(run)
	}

	sent, err := a.send(ctx, chatID, body)
	if err != nil {
		return nil, err
	}
	if !isFinalStatus(dispatch.Payload) {
		a.setStatus(run, sent)
	}
	return []string{strconv.FormatInt(sent, 10)}, nil
}

func (a *Adapter) postSegments(ctx context.Context, chatID int64, segments []string) ([]string, error) {
	ids := make([]string, 0, len(segments))
	for _, segment := range segments {
		sent, err := a.send(ctx, chatID, segment)
		if err != nil {
			// The ids of what did land are returned with the error, so the
			// outbox is not told that nothing was posted when part of it was.
			return ids, err
		}
		ids = append(ids, strconv.FormatInt(sent, 10))
	}
	return ids, nil
}

func (a *Adapter) postFile(ctx context.Context, chatID int64, file jcgateway.MessageFile) ([]string, error) {
	sent, err := a.sendDocument(ctx, chatID, file)
	if err != nil {
		return nil, err
	}
	return []string{strconv.FormatInt(sent, 10)}, nil
}

func (a *Adapter) send(ctx context.Context, chatID int64, text string) (int64, error) {
	var sent sentMessage
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
		// A link preview is a page the agent quoted an address from, expanded
		// to the size of the answer underneath it.
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if err := a.call(ctx, "sendMessage", body, &sent); err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

func (a *Adapter) edit(ctx context.Context, chatID, messageID int64, text string) (int64, error) {
	var edited sentMessage
	body := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if err := a.call(ctx, "editMessageText", body, &edited); err != nil {
		return 0, err
	}
	return messageID, nil
}

func (a *Adapter) liveStatus(run domain.RunID) (int64, bool) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	id, ok := a.live[string(run)]
	return id, ok
}

func (a *Adapter) setStatus(run domain.RunID, messageID int64) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.live[string(run)] = messageID
}

func (a *Adapter) clearStatus(run domain.RunID) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	delete(a.live, string(run))
}

// isFinalStatus reports whether a run has stopped, so its line is released
// rather than being rewritten by the next run in the same chat.
func isFinalStatus(payload string) bool {
	var status jcgateway.StatusPayload
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		return false
	}
	switch status.State {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// attachedFile is the file a dispatch carries, if it carries one.
func attachedFile(dispatch jcgateway.Dispatch) (jcgateway.MessageFile, bool) {
	if dispatch.Kind != jcgateway.DispatchMessage {
		return jcgateway.MessageFile{}, false
	}
	var payload jcgateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return jcgateway.MessageFile{}, false
	}
	if payload.File == nil || len(payload.File.Content) == 0 {
		return jcgateway.MessageFile{}, false
	}
	return *payload.File, true
}

// targetChat picks where to post. Telegram has no threads of the kind Discord
// has, so a chat is the only answer, but the field is read anyway: a binding
// written for one platform must not silently post somewhere else on another.
func targetChat(target jcgateway.ConversationRef) string {
	if target.ThreadID != "" {
		return target.ThreadID
	}
	return target.ChannelID
}

// sendDocument uploads a file, which needs a multipart body rather than JSON —
// the only request here that does.
func (a *Adapter) sendDocument(ctx context.Context, chatID int64, file jcgateway.MessageFile) (int64, error) {
	name := file.Name
	if name == "" {
		name = "answer.txt"
	}

	content := file.Content
	truncated := false
	if limit := a.maxUploadBytes(); len(content) > limit {
		content = content[:limit]
		truncated = true
	}

	var sent sentMessage
	fields := map[string]string{
		"chat_id": strconv.FormatInt(chatID, 10),
		"caption": boundCaption(describeFile(len(content), truncated)),
	}
	if err := a.upload(ctx, "sendDocument", fields, "document", name, content, &sent); err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// boundCaption keeps a caption inside what Telegram accepts. The lead of an
// answer is longer than a caption may be, and an oversized one is refused
// outright — which would lose the file the caption was describing.
func boundCaption(caption string) string {
	const maxCaptionLength = 1024

	runes := []rune(caption)
	if len(runes) <= maxCaptionLength {
		return caption
	}
	return string(runes[:maxCaptionLength-1]) + "…"
}

func describeFile(size int, truncated bool) string {
	if truncated {
		return fmt.Sprintf("the answer was too long to send whole; the first %s is attached",
			formatBytes(size))
	}
	return fmt.Sprintf("the whole answer is attached (%s)", formatBytes(size))
}

func formatBytes(size int) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d bytes", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

func (a *Adapter) maxUploadBytes() int {
	if a.config.MaxUploadBytes > 0 {
		return a.config.MaxUploadBytes
	}
	return defaultMaxUploadBytes
}

// defaultMaxUploadBytes is well under what Telegram accepts, because the point
// is to be readable rather than to be the largest possible file.
const defaultMaxUploadBytes = 4 << 20
